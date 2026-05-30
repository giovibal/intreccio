package cluster

import "github.com/giovibal/mycypher"

// Open starts a clustered node and returns it as a ready-to-use *mycypher.DB.
// The returned DB has the same API as an embedded one (Query, Explain, Close);
// writes are replicated through Raft and reads are served from the local
// replica. Closing the DB shuts the node down.
//
// This is the single public entry point for clustering. Importing this package
// is what links the Raft stack; programs that only use mycypher.Open are
// unaffected.
func Open(cfg Config) (*mycypher.DB, error) {
	node, err := newNode(cfg)
	if err != nil {
		return nil, err
	}
	return mycypher.New(node), nil
}
