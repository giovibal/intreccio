package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/rpc"
	"sync"
)

const defaultMaxIdleConns = 4

var errPoolClosed = errors.New("cluster: connection pool closed")

// connPool keeps a small set of reusable net/rpc connections per address so a
// forwarded query avoids a TCP + RPC handshake on every call. net/rpc multiplexes
// concurrent calls over a connection; the per-address free-list bounds idle
// connections and lets independent calls run in parallel. A connection that
// errors is discarded and the call retried once on a fresh one, so a stale
// pooled connection — e.g. to a voter that restarted — recovers transparently.
type connPool struct {
	maxIdle int
	mu      sync.Mutex
	idle    map[string][]*rpc.Client
	closed  bool
	// tls is nil for a plaintext cluster; otherwise outgoing connections are
	// wrapped in TLS (with a client certificate for mutual authentication).
	tls *tls.Config
}

func newConnPool(maxIdle int) *connPool { return newConnPoolTLS(maxIdle, nil) }

func newConnPoolTLS(maxIdle int, tlsClient *tls.Config) *connPool {
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	return &connPool{maxIdle: maxIdle, idle: make(map[string][]*rpc.Client), tls: tlsClient}
}

// get returns an idle connection for addr, or dials a new one.
func (p *connPool) get(addr string) (*rpc.Client, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	if conns := p.idle[addr]; len(conns) > 0 {
		c := conns[len(conns)-1]
		p.idle[addr] = conns[:len(conns)-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	if p.tls != nil {
		conn, err := tls.Dial("tcp", addr, p.tls)
		if err != nil {
			return nil, fmt.Errorf("cluster: dial %s: %w", addr, err)
		}
		return rpc.NewClient(conn), nil
	}

	c, err := rpc.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cluster: dial %s: %w", addr, err)
	}
	return c, nil
}

// put returns a healthy connection to the pool, closing it if the pool is full
// or already closed.
func (p *connPool) put(addr string, c *rpc.Client) {
	p.mu.Lock()
	if p.closed || len(p.idle[addr]) >= p.maxIdle {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.idle[addr] = append(p.idle[addr], c)
	p.mu.Unlock()
}

// call performs a forwarding RPC to addr over a pooled connection, retrying once
// on a fresh connection if a pooled one turns out to be broken. ctx bounds the
// wait for a reply: if it is done first the call returns ctx.Err() and the
// connection is dropped.
func (p *connPool) call(ctx context.Context, addr string, args *ForwardArgs) (*ForwardReply, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := p.get(addr)
		if err != nil {
			return nil, err
		}
		var reply ForwardReply
		call := c.Go("Forward.Query", args, &reply, nil)
		select {
		case <-call.Done:
			if call.Error != nil {
				_ = c.Close() // broken connection: drop it, don't return it to the pool
				lastErr = call.Error
				continue
			}
			p.put(addr, c)
			return &reply, nil
		case <-ctx.Done():
			_ = c.Close()
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("cluster: forward call: %w", lastErr)
}

// Close closes all idle connections and prevents further pooling.
func (p *connPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for addr, conns := range p.idle {
		for _, c := range conns {
			_ = c.Close()
		}
		delete(p.idle, addr)
	}
	return nil
}
