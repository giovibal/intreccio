package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/giovibal/intreccio/internal/wire"
)

var errPoolClosed = errors.New("cluster: connection pool closed")

// connPool keeps one multiplexed connection per member address. Requests to
// the same member share it, each on its own stream, so a forwarded query costs
// neither a TCP nor a TLS handshake. A connection that fails is dropped and
// redialled on the next request, so a member that restarted is reached again
// transparently.
type connPool struct {
	// tls is nil for a plaintext cluster; otherwise outgoing connections are
	// wrapped in TLS (with a client certificate for mutual authentication).
	tls *tls.Config
	ka  keepalive

	mu      sync.Mutex
	entries map[string]*poolEntry
	closed  bool
}

// poolEntry is the connection to one address, or the dial that is producing
// it. All fields are guarded by connPool.mu.
type poolEntry struct {
	conn    *muxConn
	dialing chan struct{} // non-nil while a dial is in flight; closed when it ends
	err     error         // the outcome of the last dial
}

func newConnPool(tlsClient *tls.Config) *connPool {
	return &connPool{tls: tlsClient, ka: defaultKeepalive, entries: map[string]*poolEntry{}}
}

// get returns a live connection to addr, dialling if there is none. Concurrent
// callers share a single dial, and each stops waiting when its own ctx ends.
func (p *connPool) get(ctx context.Context, addr string) (*muxConn, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed
		}
		e := p.entries[addr]
		if e == nil {
			e = &poolEntry{}
			p.entries[addr] = e
		}
		if e.conn != nil && !e.conn.broken() {
			c := e.conn
			p.mu.Unlock()
			return c, nil
		}
		e.conn = nil
		if e.dialing == nil {
			e.dialing = make(chan struct{})
			go p.dial(addr, e)
		}
		wait := e.dialing
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
		p.mu.Lock()
		c, err := e.conn, e.err
		p.mu.Unlock()
		if c != nil && !c.broken() {
			return c, nil
		}
		if err != nil {
			return nil, err // the dial failed, whoever started it
		}
		// The fresh connection broke already: go round again.
	}
}

// dial connects to addr in the background, so that the dial outlives the
// context of whichever caller happened to start it.
func (p *connPool) dial(addr string, e *poolEntry) {
	c, err := dialMux(context.Background(), addr, p.tls, p.ka)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed && c != nil {
		c.Close()
		c, err = nil, errPoolClosed
	}
	e.conn, e.err = c, err
	close(e.dialing)
	e.dialing = nil
}

// call runs req on the member at addr and returns its columns and encoded
// rows. If the connection fails mid-request the call is retried once on a
// fresh one, so a stale pooled connection recovers transparently. ctx bounds
// the whole call; its deadline travels with the request so that the member
// executing it stops too.
func (p *connPool) call(ctx context.Context, addr string, req wire.Request) ([]string, [][]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		req.Timeout = 0
		if dl, ok := ctx.Deadline(); ok {
			if req.Timeout = time.Until(dl); req.Timeout <= 0 {
				return nil, nil, context.DeadlineExceeded
			}
		}
		c, err := p.get(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		cols, rows, err := c.query(ctx, req)
		var ce *connError
		if errors.As(err, &ce) {
			lastErr = err
			continue
		}
		return cols, rows, err
	}
	return nil, nil, fmt.Errorf("cluster: forward call: %w", lastErr)
}

// Close closes every connection and fails further use of the pool.
func (p *connPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for addr, e := range p.entries {
		if e.conn != nil {
			e.conn.Close()
		}
		delete(p.entries, addr)
	}
	return nil
}
