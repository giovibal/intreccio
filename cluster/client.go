package cluster

import (
	"errors"
	"sync/atomic"

	"github.com/giovibal/intreccio/internal/storage"
)

// client is a dataless cluster member: it stores nothing and forwards every
// query to a voter. It lets extra service instances share the database without
// replicating it. It satisfies intreccio.Backend and the clustered routing
// interface (IsLeader/Local/ReadBarrier/Forward).
type client struct {
	peers []Peer // voters to contact (each with a ForwardAddr)
	next  atomic.Uint64
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
	return &client{peers: voters}, nil
}

// IsLeader is always false for a client.
func (c *client) IsLeader() bool { return false }

// Local is false: a client has no local store, so every query is forwarded.
func (c *client) Local() bool { return false }

// ReadBarrier is never invoked on a client (it is not the leader).
func (c *client) ReadBarrier() error { return nil }

// Forward sends the query to a voter, trying each in turn (round-robin with
// failover) until one answers. The contacted voter serves default reads locally
// and proxies writes/linearizable reads to the leader.
func (c *client) Forward(cypher string, params map[string]any, write, linearizable bool) ([]string, [][]any, error) {
	args := &ForwardArgs{
		Cypher:       cypher,
		Params:       sanitizeParams(params),
		Write:        write,
		Linearizable: linearizable,
	}
	start := int(c.next.Add(1))
	var lastErr error
	for i := range c.peers {
		p := c.peers[(start+i)%len(c.peers)]
		rep, err := dialAndCall(p.ForwardAddr, args)
		if err != nil {
			lastErr = err
			continue // voter unreachable: try the next one
		}
		if rep.Err != "" {
			lastErr = errors.New(rep.Err)
			continue // e.g. no leader momentarily: try another voter
		}
		return rep.Columns, desanitizeRows(rep.Rows), nil
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

// Close releases the client (nothing to release).
func (c *client) Close() error { return nil }
