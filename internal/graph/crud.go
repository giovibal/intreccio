// Package graph is the model + transactional CRUD + traversal primitives.
//
// It is the single point through which writes pass: every mutation updates the
// base record and all of its index keys (`l`, `p`, `o`, `i`) within the same
// transaction (Invariant #1). The functions operate on a storage.Txn provided by
// the caller, so multiple mutations can compose atomically in a single Update.
package graph

import (
	"errors"
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/storage"
	"github.com/giovibal/mycypher/internal/storage/codec"
)

// CreateNode creates a node with the given labels and properties (by name),
// interning their identifiers, and updates its indexes. Returns the new nodeID.
func CreateNode(txn storage.Txn, labels []string, props map[string]any) (uint64, error) {
	labelIDs := make([]uint32, 0, len(labels))
	for _, name := range labels {
		id, err := catalog.InternLabel(txn, name)
		if err != nil {
			return 0, err
		}
		labelIDs = append(labelIDs, id)
	}
	p, err := internProps(txn, props)
	if err != nil {
		return 0, err
	}
	id, err := catalog.NextNodeID(txn)
	if err != nil {
		return 0, err
	}
	rec := codec.NodeRecord{Labels: labelIDs, Props: p}
	if err := putNodeRecord(txn, id, rec); err != nil {
		return 0, err
	}
	if err := putNodeIndexes(txn, id, rec); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateEdge creates a typed edge between two existing nodes, writing the `e`
// record and the two adjacency views `o`/`i` (Invariant #4).
func CreateEdge(txn storage.Txn, typ string, src, dst uint64, props map[string]any) (uint64, error) {
	if _, err := GetNode(txn, src); err != nil {
		return 0, fmt.Errorf("edge src %d: %w", src, err)
	}
	if _, err := GetNode(txn, dst); err != nil {
		return 0, fmt.Errorf("edge dst %d: %w", dst, err)
	}
	typeID, err := catalog.InternType(txn, typ)
	if err != nil {
		return 0, err
	}
	p, err := internProps(txn, props)
	if err != nil {
		return 0, err
	}
	id, err := catalog.NextEdgeID(txn)
	if err != nil {
		return 0, err
	}
	rec := codec.EdgeRecord{Type: typeID, Src: src, Dst: dst, Props: p}
	b, err := codec.EncodeEdge(rec)
	if err != nil {
		return 0, err
	}
	if err := txn.Set(codec.EdgeKey(id), b); err != nil {
		return 0, err
	}
	if err := txn.Set(codec.OutKey(src, typeID, dst, id), nil); err != nil {
		return 0, err
	}
	if err := txn.Set(codec.InKey(dst, typeID, src, id), nil); err != nil {
		return 0, err
	}
	return id, nil
}

// SetProperty sets (or replaces) a node property, updating the record and the
// `p` index entries for the indexed labels (old→new delta).
func SetProperty(txn storage.Txn, nodeID uint64, key string, value any) error {
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	keyID, err := catalog.InternKey(txn, key)
	if err != nil {
		return err
	}
	value = normalize(value)
	old, had := node.Props[keyID]

	for _, l := range node.Labels {
		has, err := catalog.HasIndex(txn, l, keyID)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		if had && isIndexable(old) {
			ok, err := codec.PropKey(l, keyID, old, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Delete(ok); err != nil {
				return err
			}
		}
		if isIndexable(value) {
			nk, err := codec.PropKey(l, keyID, value, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Set(nk, nil); err != nil {
				return err
			}
		}
	}

	node.Props[keyID] = value
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// GetNode reads a node. Returns ErrNodeNotFound if absent.
func GetNode(txn storage.Txn, id uint64) (Node, error) {
	b, err := txn.Get(codec.NodeKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		return Node{}, ErrNodeNotFound
	}
	if err != nil {
		return Node{}, err
	}
	rec, err := codec.DecodeNode(b)
	if err != nil {
		return Node{}, fmt.Errorf("graph: decode node %d: %w", id, err)
	}
	return Node{ID: id, NodeRecord: rec}, nil
}

// GetEdge reads an edge. Returns ErrEdgeNotFound if absent.
func GetEdge(txn storage.Txn, id uint64) (Edge, error) {
	b, err := txn.Get(codec.EdgeKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		return Edge{}, ErrEdgeNotFound
	}
	if err != nil {
		return Edge{}, err
	}
	rec, err := codec.DecodeEdge(b)
	if err != nil {
		return Edge{}, fmt.Errorf("graph: decode edge %d: %w", id, err)
	}
	return Edge{ID: id, EdgeRecord: rec}, nil
}

// DeleteEdge removes an edge: the `e` record plus the two adjacency entries `o`/`i`.
func DeleteEdge(txn storage.Txn, id uint64) error {
	e, err := GetEdge(txn, id)
	if err != nil {
		return err
	}
	if err := txn.Delete(codec.OutKey(e.Src, e.Type, e.Dst, id)); err != nil {
		return err
	}
	if err := txn.Delete(codec.InKey(e.Dst, e.Type, e.Src, id)); err != nil {
		return err
	}
	return txn.Delete(codec.EdgeKey(id))
}

// DeleteNode removes a node and all its index entries (`l`, `p`). Returns
// ErrNodeHasEdges if incident edges exist: the caller must remove them first
// (DETACH DELETE semantics will arrive in Phase 7).
func DeleteNode(txn storage.Txn, id uint64) error {
	node, err := GetNode(txn, id)
	if err != nil {
		return err
	}
	has, err := hasIncidentEdges(txn, id)
	if err != nil {
		return err
	}
	if has {
		return ErrNodeHasEdges
	}
	if err := delNodeIndexes(txn, id, node.NodeRecord); err != nil {
		return err
	}
	return txn.Delete(codec.NodeKey(id))
}

func internProps(txn storage.Txn, props map[string]any) (map[uint32]any, error) {
	out := make(map[uint32]any, len(props))
	for k, v := range props {
		id, err := catalog.InternKey(txn, k)
		if err != nil {
			return nil, err
		}
		out[id] = normalize(v)
	}
	return out, nil
}

func putNodeRecord(txn storage.Txn, id uint64, rec codec.NodeRecord) error {
	b, err := codec.EncodeNode(rec)
	if err != nil {
		return err
	}
	return txn.Set(codec.NodeKey(id), b)
}

// putNodeIndexes writes the `l` entries (one per label) and `p` entries (for each
// indexed label×propKey pair whose scalar value is present).
func putNodeIndexes(txn storage.Txn, nodeID uint64, rec codec.NodeRecord) error {
	return forEachNodeIndex(txn, rec, func(key []byte) error { return txn.Set(key, nil) },
		func(label uint32) error { return txn.Set(codec.LabelKey(label, nodeID), nil) },
		nodeID)
}

// delNodeIndexes removes the same entries written by putNodeIndexes.
func delNodeIndexes(txn storage.Txn, nodeID uint64, rec codec.NodeRecord) error {
	return forEachNodeIndex(txn, rec, func(key []byte) error { return txn.Delete(key) },
		func(label uint32) error { return txn.Delete(codec.LabelKey(label, nodeID)) },
		nodeID)
}

// forEachNodeIndex applies onLabel for every label and onProp for every `p` entry
// to maintain, so put/del share the enumeration (single source of truth).
func forEachNodeIndex(txn storage.Txn, rec codec.NodeRecord, onProp func(key []byte) error, onLabel func(label uint32) error, nodeID uint64) error {
	for _, l := range rec.Labels {
		if err := onLabel(l); err != nil {
			return err
		}
		for k, v := range rec.Props {
			if !isIndexable(v) {
				continue
			}
			has, err := catalog.HasIndex(txn, l, k)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			pk, err := codec.PropKey(l, k, v, nodeID)
			if err != nil {
				return err
			}
			if err := onProp(pk); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasIncidentEdges(txn storage.Txn, nodeID uint64) (bool, error) {
	for _, prefix := range [][]byte{codec.OutPrefixAll(nodeID), codec.InPrefixAll(nodeID)} {
		it := txn.Scan(prefix)
		valid := it.Valid()
		if err := it.Close(); err != nil {
			return false, err
		}
		if valid {
			return true, nil
		}
	}
	return false, nil
}
