package graph

import (
	"errors"

	"github.com/giovibal/mycypher/internal/storage/codec"
)

// Errori sentinella del graph layer.
var (
	ErrNodeNotFound = errors.New("graph: nodo non trovato")
	ErrEdgeNotFound = errors.New("graph: arco non trovato")
	ErrNodeHasEdges = errors.New("graph: il nodo ha archi incidenti")
)

// Node è un nodo materializzato: ID più il record (label e proprietà come ID
// interni del dizionario).
type Node struct {
	ID uint64
	codec.NodeRecord
}

// Edge è un arco materializzato: ID più il record (tipo, src, dst, proprietà).
type Edge struct {
	ID uint64
	codec.EdgeRecord
}

// EdgeRef è la vista leggera di un arco ricavata dalla sola chiave di adiacenza
// (`o`/`i`), senza leggere il record `e`. Sufficiente per il traversal quando non
// servono le proprietà dell'arco (DESIGN §9).
type EdgeRef struct {
	ID   uint64
	Type uint32
	Src  uint64
	Dst  uint64
}
