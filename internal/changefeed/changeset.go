// Package changefeed turns committed writes into logical change sets and
// decides which registered watchers they concern. It is the engine-side half of
// live queries (ADR 0010): the capture derives a ChangeSet from the physical
// write-set of a transaction, and the Hub wakes the watchers whose footprint
// the ChangeSet intersects. Evaluating a woken query is the caller's business.
package changefeed

import "github.com/giovibal/intreccio/internal/storage/codec"

// ChangeSet is the logical effect of one committed write transaction: the node
// and relationship records it created, changed or deleted. Records it rewrote
// with identical contents are not listed.
type ChangeSet struct {
	Nodes []NodeChange
	Edges []EdgeChange
}

// Empty reports whether the transaction changed no node or relationship.
func (cs *ChangeSet) Empty() bool { return len(cs.Nodes) == 0 && len(cs.Edges) == 0 }

// NodeChange is one node's state before and after the transaction. Before is
// nil when the node was created, After is nil when it was deleted.
type NodeChange struct {
	ID     uint64
	Before *codec.NodeRecord
	After  *codec.NodeRecord
}

// EdgeChange is one relationship's state before and after the transaction, with
// the same nil convention as NodeChange. Both are set when a property of the
// relationship changed; its type and end nodes never change.
type EdgeChange struct {
	ID     uint64
	Before *codec.EdgeRecord
	After  *codec.EdgeRecord
}
