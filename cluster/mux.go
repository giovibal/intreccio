package cluster

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/giovibal/intreccio/internal/wire"
)

const (
	// dialTimeout bounds establishing a connection to another member.
	dialTimeout = 5 * time.Second
	// handshakeTimeout bounds the TLS and protocol handshakes.
	handshakeTimeout = 5 * time.Second
	// writeTimeout bounds writing one frame: a peer that stops reading must
	// not hold every stream of the connection hostage.
	writeTimeout = 10 * time.Second
	// readBufferSize is the buffer in front of a connection's frame reader.
	readBufferSize = 64 << 10
)

// keepalive is how a connection detects a peer that died without closing: the
// dialling side sends a Ping every interval, the other side answers, and each
// side gives up on a connection that stays silent for timeout.
type keepalive struct {
	interval time.Duration
	timeout  time.Duration
}

var defaultKeepalive = keepalive{interval: 3 * time.Second, timeout: 10 * time.Second}

// connError marks a failure of the connection itself, as opposed to an error
// the peer reported or the caller's context ending. The request may or may not
// have reached the peer.
type connError struct{ err error }

func (e *connError) Error() string { return "cluster: connection: " + e.err.Error() }
func (e *connError) Unwrap() error { return e.err }

var errConnClosed = errors.New("closed")

// muxConn is the dialling side of one connection to another member. Any number
// of requests run over it concurrently, each on its own stream.
type muxConn struct {
	conn net.Conn
	ka   keepalive

	wmu sync.Mutex // serializes frame writes

	mu      sync.Mutex
	streams map[uint32]*muxStream
	nextID  uint32
	err     error // why the connection failed; set before closed is closed

	closed    chan struct{}
	closeOnce sync.Once
}

// muxStream is the receiving end of one request's reply.
type muxStream struct {
	id     uint32
	frames chan wire.Frame
	done   chan struct{} // closed when the caller stops listening
}

// dialMux connects to addr, completes the handshakes and starts the
// connection's reader and keepalive.
func dialMux(ctx context.Context, addr string, tlsCfg *tls.Config, ka keepalive) (*muxConn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var (
		conn net.Conn
		err  error
	)
	if tlsCfg != nil {
		d := tls.Dialer{Config: tlsCfg}
		conn, err = d.DialContext(ctx, "tcp", addr)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("cluster: dial %s: %w", addr, err)
	}

	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := wire.WriteHandshake(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cluster: handshake with %s: %w", addr, err)
	}
	if err := wire.ReadHandshake(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cluster: handshake with %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{})

	c := &muxConn{
		conn:    conn,
		ka:      ka,
		streams: map[uint32]*muxStream{},
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	go c.pingLoop()
	return c, nil
}

// fail closes the connection, recording why. Every stream sees it.
func (c *muxConn) fail(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.closed)
		_ = c.conn.Close()
	})
}

// Close closes the connection; in-flight requests fail.
func (c *muxConn) Close() { c.fail(errConnClosed) }

func (c *muxConn) broken() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// failure returns the connection's failure as a connError.
func (c *muxConn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &connError{err: c.err}
}

func (c *muxConn) write(stream uint32, typ wire.FrameType, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := wire.WriteFrame(c.conn, stream, typ, payload); err != nil {
		// A partial frame leaves the byte stream unusable.
		c.fail(err)
		return c.failure()
	}
	return nil
}

// readLoop dispatches incoming frames to their streams until the connection
// fails. It never waits on a caller that has stopped listening.
func (c *muxConn) readLoop() {
	r := bufio.NewReaderSize(c.conn, readBufferSize)
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.ka.timeout))
		f, err := wire.ReadFrame(r)
		if err != nil {
			c.fail(err)
			return
		}
		if f.Stream == 0 {
			continue // Pong: its arrival already refreshed the read deadline
		}
		c.mu.Lock()
		s := c.streams[f.Stream]
		c.mu.Unlock()
		if s == nil {
			continue // a reply to a request the caller has given up on
		}
		select {
		case s.frames <- f:
		case <-s.done:
		}
	}
}

func (c *muxConn) pingLoop() {
	t := time.NewTicker(c.ka.interval)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			if c.write(0, wire.FramePing, nil) != nil {
				return
			}
		}
	}
}

func (c *muxConn) open() (*muxStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, &connError{err: c.err}
	}
	for {
		c.nextID++
		if _, used := c.streams[c.nextID]; c.nextID != 0 && !used {
			break
		}
	}
	s := &muxStream{id: c.nextID, frames: make(chan wire.Frame), done: make(chan struct{})}
	c.streams[s.id] = s
	return s, nil
}

func (c *muxConn) release(s *muxStream) {
	c.mu.Lock()
	delete(c.streams, s.id)
	c.mu.Unlock()
	close(s.done)
}

// query runs one request and collects its reply. The error is a *connError
// when the connection failed, a *wire.Error when the peer reported a failure,
// and ctx's error when ctx ended first — in which case the peer is told to
// abandon the request.
func (c *muxConn) query(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	s, err := c.open()
	if err != nil {
		return nil, nil, err
	}
	defer c.release(s)

	if err := c.write(s.id, wire.FrameQuery, wire.EncodeRequest(req)); err != nil {
		return nil, nil, err
	}

	var (
		cols []string
		rows [][]byte
	)
	for {
		select {
		case <-ctx.Done():
			_ = c.write(s.id, wire.FrameCancel, nil) // best effort
			return nil, nil, ctx.Err()
		case <-c.closed:
			return nil, nil, c.failure()
		case f := <-s.frames:
			switch f.Type {
			case wire.FrameResult:
				if cols, err = wire.DecodeColumns(f.Payload); err != nil {
					c.fail(err)
					return nil, nil, c.failure()
				}
			case wire.FrameRows:
				chunk, err := wire.DecodeRows(f.Payload)
				if err != nil {
					c.fail(err)
					return nil, nil, c.failure()
				}
				rows = append(rows, chunk...)
			case wire.FrameEnd:
				return cols, rows, nil
			case wire.FrameError:
				remote, err := wire.DecodeError(f.Payload)
				if err != nil {
					c.fail(err)
					return nil, nil, c.failure()
				}
				return nil, nil, remote
			default:
				c.fail(fmt.Errorf("%w: unexpected frame type %d in a reply", wire.ErrProtocol, f.Type))
				return nil, nil, c.failure()
			}
		}
	}
}
