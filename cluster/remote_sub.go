package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/wire"
)

const (
	// defaultFailoverTimeout bounds how long a subscription that lost its
	// voter keeps looking for another before it gives up.
	defaultFailoverTimeout = 30 * time.Second
	// failoverBackoff is the pause between rounds over the voters, doubling up
	// to failoverBackoffMax.
	failoverBackoff    = 100 * time.Millisecond
	failoverBackoffMax = 2 * time.Second
)

// remoteSub is a dataless client's end of a live query served by a voter. It
// hides which voter that is: when the serving voter is lost it subscribes to
// another one and turns the fresh result into the net difference from what the
// application already has.
//
// Flow control is one batch at a time: the voter sends a batch and waits for an
// Ack, which is sent when the consumer takes the batch (Recv). A consumer that
// is slow therefore holds the query on the voter, where commits coalesce, and
// never fills the connection it shares with the client's other requests.
type remoteSub struct {
	ctx context.Context
	c   *client
	req wire.Request

	out  chan wire.Batch // batches ready for Recv
	done chan struct{}   // closed when the subscription has ended
	err  error           // why; set before done is closed

	// The fields below belong to the pump goroutine.
	conn   *muxConn
	stream *muxStream // nil while not attached to a voter
	// mirror is the result as of the last batch received, whether or not the
	// consumer has taken it yet: the reference a new voter's result is
	// compared with.
	mirror changefeed.Mirror
	// lastIndex is the highest applied index seen in a batch. A new voter must
	// have applied at least this much, so the result never moves backwards.
	lastIndex uint64
}

var _ wire.BatchStream = (*remoteSub)(nil)

// SubscribeRemote starts a live query on a voter. It returns once the query's
// first result has arrived, so an invalid query fails here; the stream then
// lives until ctx ends or no voter can serve it any more.
func (c *client) SubscribeRemote(ctx context.Context, req wire.Request) ([]string, wire.BatchStream, error) {
	rs := &remoteSub{
		ctx:  ctx,
		c:    c,
		req:  req,
		out:  make(chan wire.Batch),
		done: make(chan struct{}),
	}
	cols, snapshot, err := rs.establish(true)
	if err != nil {
		return nil, nil, err
	}
	rs.mirror.Reset(snapshot.Added)
	rs.lastIndex = snapshot.Index
	go rs.pump(wire.Batch{Initial: true, Index: snapshot.Index, Added: snapshot.Added})
	return cols, rs, nil
}

// Recv returns the next batch. Taking a batch is what acknowledges it.
func (rs *remoteSub) Recv() (wire.Batch, error) {
	select {
	case b := <-rs.out:
		return b, nil
	case <-rs.done:
		return wire.Batch{}, rs.err
	}
}

// pump is the subscription's goroutine. In every state in which it is attached
// to a voter it keeps receiving from the stream, so it never stalls the
// connection's reader.
func (rs *remoteSub) pump(first wire.Batch) {
	defer close(rs.done)
	defer func() {
		if rs.stream != nil {
			_ = rs.conn.write(rs.stream.id, wire.FrameCancel, nil) // best effort
			rs.detach()
		}
	}()

	pending := &first // received, not yet taken by the consumer
	var partial wire.Batch
	for {
		switch {
		case rs.stream == nil && pending != nil:
			// Lost the voter with a batch in hand: hand it over first.
			select {
			case rs.out <- *pending:
				pending = nil
			case <-rs.ctx.Done():
				return
			}

		case rs.stream == nil:
			_, snapshot, err := rs.establish(false)
			if err != nil {
				if rs.ctx.Err() == nil {
					rs.err = err
				}
				return
			}
			partial = wire.Batch{}
			addedIdx, removed := rs.mirror.Reset(snapshot.Added)
			rs.lastIndex = max(rs.lastIndex, snapshot.Index)
			if len(addedIdx) == 0 && len(removed) == 0 {
				rs.ack() // the new voter's result is what the consumer has
				continue
			}
			diff := wire.Batch{Index: snapshot.Index, Removed: removed}
			for _, i := range addedIdx {
				diff.Added = append(diff.Added, snapshot.Added[i])
			}
			pending = &diff

		case pending != nil:
			select {
			case rs.out <- *pending:
				pending = nil
				rs.ack()
			case f := <-rs.stream.frames:
				// The voter waits for the Ack: anything it sends now ends the
				// stream. The batch in hand is still delivered.
				if fatal := rs.streamEnded(f); fatal != nil {
					rs.err = fatal
					return
				}
			case <-rs.conn.closed:
				rs.detach()
			case <-rs.ctx.Done():
				return
			}

		default:
			select {
			case f := <-rs.stream.frames:
				if f.Type != wire.FrameChange {
					if fatal := rs.streamEnded(f); fatal != nil {
						rs.err = fatal
						return
					}
					continue
				}
				part, err := wire.DecodeBatch(f.Payload)
				if err != nil {
					rs.conn.fail(err)
					rs.detach()
					continue
				}
				partial.Merge(part)
				if part.More {
					continue
				}
				rs.mirror.Apply(partial.Added, partial.Removed)
				rs.lastIndex = max(rs.lastIndex, partial.Index)
				batch := partial
				partial = wire.Batch{}
				pending = &batch
			case <-rs.conn.closed:
				rs.detach()
			case <-rs.ctx.Done():
				return
			}
		}
	}
}

// ack tells the voter the consumer is ready for the next batch.
func (rs *remoteSub) ack() {
	if rs.stream == nil {
		return
	}
	if rs.conn.write(rs.stream.id, wire.FrameAck, nil) != nil {
		rs.detach()
	}
}

// detach forgets the current voter.
func (rs *remoteSub) detach() {
	if rs.stream != nil {
		rs.conn.release(rs.stream)
	}
	rs.conn, rs.stream = nil, nil
}

// streamEnded handles a frame by which the voter ends the stream. It returns
// the error to end the subscription with, or nil if another voter should be
// tried.
func (rs *remoteSub) streamEnded(f wire.Frame) error {
	conn := rs.conn
	rs.detach()
	switch f.Type {
	case wire.FrameEnd:
		return nil
	case wire.FrameError:
		remote, err := wire.DecodeError(f.Payload)
		if err != nil {
			conn.fail(err)
			return nil
		}
		if retryable(remote) {
			return nil
		}
		return remote
	default:
		conn.fail(fmt.Errorf("%w: unexpected frame type %d on a subscription", wire.ErrProtocol, f.Type))
		return nil
	}
}

// retryable reports whether err says "not this voter, not now" rather than
// something about the query itself.
func retryable(err error) bool {
	var remote *wire.Error
	if errors.As(err, &remote) {
		return remote.Code == wire.CodeUnavailable || remote.Code == wire.CodeBehind
	}
	return true // dial, handshake and connection failures
}

// establish attaches the subscription to a voter and returns the columns and
// the voter's current result. The first time it makes a single round over the
// voters. After that — a failover — it keeps trying for the client's failover
// timeout, asking each voter to have caught up with what was already seen.
func (rs *remoteSub) establish(first bool) ([]string, wire.Batch, error) {
	after := rs.lastIndex
	deadline := time.Now().Add(rs.c.failoverTimeout)
	backoff := failoverBackoff
	for {
		var lastErr error
		onlyBehind := after > 0
		start := int(rs.c.next.Add(1))
		for i := range rs.c.peers {
			peer := rs.c.peers[(start+i)%len(rs.c.peers)]
			cols, snapshot, err := rs.attach(peer.ForwardAddr, after)
			if err == nil {
				return cols, snapshot, nil
			}
			if rs.ctx.Err() != nil {
				return nil, wire.Batch{}, rs.ctx.Err()
			}
			if !retryable(err) {
				return nil, wire.Batch{}, err
			}
			var remote *wire.Error
			if !errors.As(err, &remote) || remote.Code != wire.CodeBehind {
				onlyBehind = false
			}
			lastErr = err
		}
		if first {
			return nil, wire.Batch{}, lastErr
		}
		if onlyBehind {
			// Every voter answered and none has what this subscription saw: the
			// cluster itself went back (a restore). Follow it rather than stall.
			after = 0
			continue
		}
		if time.Now().After(deadline) {
			return nil, wire.Batch{}, fmt.Errorf("cluster: no voter could take over the subscription: %w", lastErr)
		}
		select {
		case <-rs.ctx.Done():
			return nil, wire.Batch{}, rs.ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, failoverBackoffMax)
	}
}

// attach opens a subscription stream on the voter at addr and waits for its
// first result. On success the stream becomes the current one.
func (rs *remoteSub) attach(addr string, after uint64) ([]string, wire.Batch, error) {
	conn, err := rs.c.pool.get(rs.ctx, addr)
	if err != nil {
		return nil, wire.Batch{}, err
	}
	s, err := conn.open()
	if err != nil {
		return nil, wire.Batch{}, err
	}
	payload := wire.EncodeSubscribe(wire.SubscribeRequest{Request: rs.req, AfterIndex: after})
	if err := conn.write(s.id, wire.FrameSubscribe, payload); err != nil {
		conn.release(s)
		return nil, wire.Batch{}, err
	}

	var (
		cols     []string
		snapshot wire.Batch
	)
	for {
		select {
		case <-rs.ctx.Done():
			_ = conn.write(s.id, wire.FrameCancel, nil) // best effort
			conn.release(s)
			return nil, wire.Batch{}, rs.ctx.Err()
		case <-conn.closed:
			conn.release(s)
			return nil, wire.Batch{}, conn.failure()
		case f := <-s.frames:
			switch f.Type {
			case wire.FrameResult:
				if cols, err = wire.DecodeColumns(f.Payload); err != nil {
					conn.fail(err)
					conn.release(s)
					return nil, wire.Batch{}, conn.failure()
				}
			case wire.FrameChange:
				part, err := wire.DecodeBatch(f.Payload)
				if err != nil {
					conn.fail(err)
					conn.release(s)
					return nil, wire.Batch{}, conn.failure()
				}
				snapshot.Merge(part)
				if !part.More {
					rs.conn, rs.stream = conn, s
					return cols, snapshot, nil
				}
			case wire.FrameError:
				conn.release(s)
				remote, err := wire.DecodeError(f.Payload)
				if err != nil {
					conn.fail(err)
					return nil, wire.Batch{}, conn.failure()
				}
				return nil, wire.Batch{}, remote
			default: // End, or a frame that has no business here
				conn.release(s)
				return nil, wire.Batch{}, &wire.Error{Code: wire.CodeUnavailable, Msg: "cluster: the voter ended the subscription"}
			}
		}
	}
}
