package cluster

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/giovibal/intreccio/internal/wire"
)

// requestHandler runs one request on this member and returns its columns and
// encoded rows. ctx ends when the requester cancels, disconnects or its
// deadline passes.
type requestHandler func(ctx context.Context, req wire.Request) ([]string, [][]byte, error)

// subscriptionHandler runs one live query on this member, sending its output to
// sink until ctx ends or the query fails.
type subscriptionHandler func(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error

// forwardServer is the accepting side of the forwarding protocol: it serves
// the requests other members send to this one.
type forwardServer struct {
	ln        net.Listener
	handler   requestHandler
	subscribe subscriptionHandler // nil: this member serves no live queries
	ka        keepalive

	mu     sync.Mutex
	conns  map[*serverConn]struct{}
	closed bool

	wg sync.WaitGroup // the accept loop, every connection and every request
}

func newForwardServer(ln net.Listener, handler requestHandler) *forwardServer {
	return &forwardServer{ln: ln, handler: handler, ka: defaultKeepalive, conns: map[*serverConn]struct{}{}}
}

// start begins accepting connections.
func (s *forwardServer) start() {
	s.wg.Add(1)
	go s.accept()
}

func (s *forwardServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // the listener was closed
		}
		ctx, cancel := context.WithCancel(context.Background())
		c := &serverConn{srv: s, conn: conn, ctx: ctx, cancel: cancel, streams: map[uint32]*serverStream{}}

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			cancel()
			_ = conn.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go c.run()
	}
}

// shutdown stops accepting, closes every connection and cancels the requests
// in flight. It does not wait for them: see wait.
func (s *forwardServer) shutdown() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		c.close()
	}
	return err
}

// wait blocks until every goroutine of the server has returned. Call it after
// shutdown and once whatever a request may be blocked on has been released.
func (s *forwardServer) wait() { s.wg.Wait() }

// serverConn is one accepted connection.
type serverConn struct {
	srv  *forwardServer
	conn net.Conn

	// ctx is the parent of every request on the connection; it ends with it.
	ctx    context.Context
	cancel context.CancelFunc

	wmu sync.Mutex // serializes frame writes

	mu      sync.Mutex
	streams map[uint32]*serverStream // requests and subscriptions in flight
}

// serverStream is one stream being served.
type serverStream struct {
	cancel context.CancelFunc
	// acks receives the subscriber's acknowledgements on a subscription
	// stream. It holds at most one: an Ack means "ready for the next batch".
	acks chan struct{}
}

func (c *serverConn) close() {
	c.cancel()
	_ = c.conn.Close()
}

func (c *serverConn) run() {
	defer c.srv.wg.Done()
	defer func() {
		c.close()
		c.srv.mu.Lock()
		delete(c.srv.conns, c)
		c.srv.mu.Unlock()
	}()

	// The requester speaks first. Answer even a version mismatch, so that it
	// learns which version this member runs.
	_ = c.conn.SetDeadline(time.Now().Add(handshakeTimeout))
	err := wire.ReadHandshake(c.conn)
	var mismatch *wire.VersionError
	if err != nil && !errors.As(err, &mismatch) {
		return // not this protocol (or a failed TLS handshake)
	}
	if wire.WriteHandshake(c.conn) != nil || err != nil {
		return
	}
	_ = c.conn.SetDeadline(time.Time{})

	r := bufio.NewReaderSize(c.conn, readBufferSize)
	for {
		// The requester pings regularly: silence means it is gone.
		_ = c.conn.SetReadDeadline(time.Now().Add(c.srv.ka.timeout))
		f, err := wire.ReadFrame(r)
		if err != nil {
			return
		}
		switch f.Type {
		case wire.FramePing:
			if c.write(0, wire.FramePong, nil) != nil {
				return
			}
		case wire.FrameQuery:
			if !c.begin(f, c.serve) {
				return
			}
		case wire.FrameSubscribe:
			if !c.begin(f, c.serveSubscription) {
				return
			}
		case wire.FrameAck:
			if st := c.stream(f.Stream); st != nil {
				select {
				case st.acks <- struct{}{}:
				default: // an Ack is already pending
				}
			}
		case wire.FrameCancel:
			if st := c.stream(f.Stream); st != nil {
				st.cancel()
			}
		default:
			return // a frame only this side may send: protocol violation
		}
	}
}

func (c *serverConn) stream(id uint32) *serverStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

// begin starts serving the stream opened by f on its own goroutine. It reports
// false on a protocol violation.
func (c *serverConn) begin(f wire.Frame, serve func(ctx context.Context, st *serverStream, stream uint32, payload []byte)) bool {
	if f.Stream == 0 {
		return false
	}
	ctx, cancel := context.WithCancel(c.ctx)
	st := &serverStream{cancel: cancel, acks: make(chan struct{}, 1)}
	c.mu.Lock()
	if _, dup := c.streams[f.Stream]; dup {
		c.mu.Unlock()
		cancel()
		return false
	}
	c.streams[f.Stream] = st
	c.mu.Unlock()

	c.srv.wg.Add(1)
	go func() {
		defer c.srv.wg.Done()
		defer func() {
			c.mu.Lock()
			delete(c.streams, f.Stream)
			c.mu.Unlock()
			cancel()
		}()
		serve(ctx, st, f.Stream, f.Payload)
	}()
	return true
}

func (c *serverConn) serve(ctx context.Context, _ *serverStream, stream uint32, payload []byte) {
	req, err := wire.DecodeRequest(payload)
	if err != nil {
		c.reply(ctx, stream, nil, nil, err)
		return
	}
	if req.Timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, req.Timeout)
		defer stop()
	}
	cols, rows, err := c.srv.handler(ctx, req)
	var coded *wire.Error
	if err != nil && req.Timeout > 0 && !errors.As(err, &coded) && errors.Is(err, context.DeadlineExceeded) {
		// The requester's own deadline, reached here first: keep its identity.
		// A timeout of this member's making (a proxy wait, say) stays uncoded,
		// so that a client may still try another voter.
		err = &wire.Error{Code: wire.CodeDeadlineExceeded, Msg: err.Error()}
	}
	if err == nil && req.Write {
		// The write is applied. If its result cannot be sent back, the
		// requester must not take that for a write that did not happen.
		if err = oversizedRow(rows); err != nil {
			err = unknownOutcome("the write was applied but its result cannot be returned: %v", err)
		}
	}
	c.reply(ctx, stream, cols, rows, err)
}

// serveSubscription runs a live query for the subscriber on the other end. It
// ends when the subscriber cancels or disconnects, or when the query fails, in
// which case the subscriber is told why.
func (c *serverConn) serveSubscription(ctx context.Context, st *serverStream, stream uint32, payload []byte) {
	req, err := wire.DecodeSubscribe(payload)
	if err == nil && c.srv.subscribe == nil {
		err = &wire.Error{Code: wire.CodeUnavailable, Msg: "cluster: this member serves no live queries"}
	}
	if err == nil {
		err = c.srv.subscribe(ctx, req, &streamSink{c: c, ctx: ctx, st: st, stream: stream})
	}
	if errors.Is(context.Cause(ctx), context.Canceled) {
		return // cancelled or disconnected: nobody is listening
	}
	if err == nil {
		_ = c.write(stream, wire.FrameEnd, nil)
		return
	}
	c.replyError(stream, err)
}

// streamSink sends a live query's output down one stream.
type streamSink struct {
	c      *serverConn
	ctx    context.Context
	st     *serverStream
	stream uint32
}

func (s *streamSink) Columns(cols []string) error {
	return s.c.write(s.stream, wire.FrameResult, wire.EncodeColumns(cols))
}

// Batch sends b, split over as many Change frames as its size needs, then
// waits for the subscriber's Ack. That wait is the flow control: a subscriber
// that is slow to consume holds the live query here, where further commits
// coalesce, instead of filling the connection it shares with other streams.
func (s *streamSink) Batch(b wire.Batch) error {
	for _, part := range wire.SplitBatch(b, wire.RowsChunkTarget) {
		payload := wire.EncodeBatch(part)
		if len(payload) > wire.MaxPayload {
			return fmt.Errorf("cluster: a result row exceeds the %d-byte frame limit", wire.MaxPayload)
		}
		if err := s.c.write(s.stream, wire.FrameChange, payload); err != nil {
			return err
		}
	}
	select {
	case <-s.st.acks:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// replyError sends err as an Error frame, keeping its code if it has one.
func (c *serverConn) replyError(stream uint32, err error) {
	var remote *wire.Error
	if !errors.As(err, &remote) {
		remote = &wire.Error{Code: wire.CodeGeneric, Msg: err.Error()}
	}
	_ = c.write(stream, wire.FrameError, wire.EncodeError(remote))
}

// reply sends the outcome of a request: an Error frame, or the columns, the
// rows in chunks and an End frame.
func (c *serverConn) reply(ctx context.Context, stream uint32, cols []string, rows [][]byte, err error) {
	if errors.Is(context.Cause(ctx), context.Canceled) {
		return // cancelled or disconnected: nobody is listening for the reply
	}
	if err == nil {
		err = oversizedRow(rows)
	}
	if err != nil {
		c.replyError(stream, err)
		return
	}
	if c.write(stream, wire.FrameResult, wire.EncodeColumns(cols)) != nil {
		return
	}
	for _, chunk := range wire.ChunkRows(rows, wire.RowsChunkTarget) {
		if c.write(stream, wire.FrameRows, wire.EncodeRows(chunk)) != nil {
			return
		}
	}
	_ = c.write(stream, wire.FrameEnd, nil)
}

// oversizedRow reports a row that does not fit in a frame.
func oversizedRow(rows [][]byte) error {
	for _, row := range rows {
		if len(row) > wire.MaxPayload-16 {
			return fmt.Errorf("cluster: a result row of %d bytes exceeds the %d-byte frame limit", len(row), wire.MaxPayload)
		}
	}
	return nil
}

func (c *serverConn) write(stream uint32, typ wire.FrameType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := wire.WriteFrame(c.conn, stream, typ, payload); err != nil {
		c.close() // a partial frame leaves the byte stream unusable
		return err
	}
	return nil
}
