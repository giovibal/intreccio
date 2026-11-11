package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"sync/atomic"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/wire"
)

// client is a dataless cluster member: it stores nothing and forwards every
// query to a voter. It lets extra service instances share the database without
// replicating it. It satisfies intreccio.Backend and the clustered routing
// interface (IsLeader/Local/ReadBarrier/Forward).
type client struct {
	peers []Peer // voters to contact (each with a ForwardAddr)
	next  atomic.Uint64
	pool  *connPool
}

func newClient(cfg Config) (*client, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("cluster: client requires a NodeID")
	}
	var voters []Peer
	for _, p := range cfg.Peers {
		if p.ForwardAddr != "" {
			voters = append(voters, p)
		}
	}
	if len(voters) == 0 {
		return nil, errors.New("cluster: client requires at least one peer with a ForwardAddr")
	}

	var tlsClient *tls.Config
	if cfg.TLS != nil {
		var err error
		_, tlsClient, err = cfg.TLS.configs()
		if err != nil {
			return nil, err
		}
	}
	return &client{peers: voters, pool: newConnPool(tlsClient)}, nil
}

// IsLeader is always false for a client.
func (c *client) IsLeader() bool { return false }

// Local is false: a client has no local store, so every query is forwarded.
func (c *client) Local() bool { return false }

// ReadBarrier is never invoked on a client (it is not the leader).
func (c *client) ReadBarrier() error { return nil }

// Forward sends the request to a voter, trying each in turn (round-robin with
// failover) until one answers. The contacted voter serves default reads locally
// and proxies writes/linearizable reads to the leader.
func (c *client) Forward(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	start := int(c.next.Add(1))
	var lastErr error
	for i := range c.peers {
		p := c.peers[(start+i)%len(c.peers)]
		cols, rows, err := c.pool.call(ctx, p.ForwardAddr, req)
		if err == nil {
			return cols, rows, nil
		}
		if ctx.Err() != nil {
			return nil, nil, err // the caller gave up: no point trying elsewhere
		}
		var remote *wire.Error
		if errors.As(err, &remote) && remote.Code != wire.CodeGeneric {
			return nil, nil, err // a definite answer about the query itself
		}
		// Voter unreachable, or e.g. no leader momentarily: try the next one.
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("cluster: no voters available")
	}
	return nil, nil, lastErr
}

// View is unsupported on a client; reads are forwarded by the router.
func (c *client) View(func(storage.Txn) error) error {
	return errors.New("cluster: client has no local store")
}

// ApplyWrite is unsupported on a client; writes are forwarded by the router.
func (c *client) ApplyWrite(func(storage.Txn) (any, error)) (any, error) {
	return nil, errors.New("cluster: client has no local store")
}

// Close releases the client's pooled connections.
func (c *client) Close() error { return c.pool.Close() }
