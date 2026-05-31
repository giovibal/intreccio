package cluster

import (
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
}

func newConnPool(maxIdle int) *connPool {
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	return &connPool{maxIdle: maxIdle, idle: make(map[string][]*rpc.Client)}
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
// on a fresh connection if a pooled one turns out to be broken.
func (p *connPool) call(addr string, args *ForwardArgs) (*ForwardReply, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		c, err := p.get(addr)
		if err != nil {
			return nil, err
		}
		var reply ForwardReply
		if err := c.Call("Forward.Query", args, &reply); err != nil {
			_ = c.Close() // broken connection: drop it, don't return it to the pool
			lastErr = err
			continue
		}
		p.put(addr, c)
		return &reply, nil
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
