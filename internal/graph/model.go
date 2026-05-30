package graph

import (
	"errors"

	"github.com/giovibal/intreccio/internal/storage/codec"
)

// Graph layer sentinel errors.
var (
	ErrNodeNotFound = errors.New("graph: node not found")
	ErrEdgeNotFound = errors.New("graph: edge not found")
	ErrNodeHasEdges = errors.New("graph: node has incident edges")
)

// Node is a materialized node: ID plus the record (labels and properties as
// internal dictionary IDs).
type Node struct {
	ID uint64
	codec.NodeRecord
}

// Edge is a materialized edge: ID plus the record (type, src, dst, properties).
type Edge struct {
	ID uint64
	codec.EdgeRecord
}

// EdgeRef is the lightweight view of an edge derived from the adjacency key alone
// (`o`/`i`), without reading the `e` record. Sufficient for traversal when the
// edge properties are not needed (DESIGN §9).
type EdgeRef struct {
	ID   uint64
	Type uint32
	Src  uint64
	Dst  uint64
}
