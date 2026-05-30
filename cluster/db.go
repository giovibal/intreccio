package cluster

import "github.com/giovibal/intreccio"

// Open starts a clustered node and returns it as a ready-to-use *intreccio.DB.
// The returned DB has the same API as an embedded one (Query, Explain, Close).
//
// A RoleVoter node holds a replica and participates in consensus: writes are
// replicated through Raft and reads are served locally. A RoleClient node holds
// no data and forwards every query to a voter — useful for extra service
// instances that should share the database without replicating it.
//
// This is the single public entry point for clustering. Importing this package
// is what links the Raft stack; programs that only use intreccio.Open are
// unaffected.
func Open(cfg Config) (*intreccio.DB, error) {
	if cfg.Role == RoleClient {
		c, err := newClient(cfg)
		if err != nil {
			return nil, err
		}
		return intreccio.New(c), nil
	}
	node, err := newNode(cfg)
	if err != nil {
		return nil, err
	}
	return intreccio.New(node), nil
}
