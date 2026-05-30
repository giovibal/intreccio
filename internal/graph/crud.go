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

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
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

	if value == nil {
		delete(node.Props, keyID)
	} else {
		node.Props[keyID] = value
	}
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

// RemoveProperty removes a property from a node, updating record and `p`
// index entries for any indexed (label, propKey) pair in the same transaction.
// Removing a non-existent property is a no-op.
func RemoveProperty(txn storage.Txn, nodeID uint64, key string) error {
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	keyID, found, err := catalog.LookupKey(txn, key)
	if err != nil {
		return err
	}
	if !found {
		return nil // unknown key globally → no entries anywhere
	}
	old, had := node.Props[keyID]
	if !had {
		return nil
	}
	delete(node.Props, keyID)
	for _, l := range node.Labels {
		has, err := catalog.HasIndex(txn, l, keyID)
		if err != nil {
			return err
		}
		if !has || !isIndexable(old) {
			continue
		}
		pk, err := codec.PropKey(l, keyID, old, nodeID)
		if err != nil {
			return err
		}
		if err := txn.Delete(pk); err != nil {
			return err
		}
	}
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// AddLabels adds labels to a node, updating the record, the `l` index entries
// and any `p` index entries induced by the new (label, propKey) combinations.
// Duplicate labels are ignored.
func AddLabels(txn storage.Txn, nodeID uint64, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	existing := make(map[uint32]bool, len(node.Labels))
	for _, l := range node.Labels {
		existing[l] = true
	}
	var added []uint32
	for _, name := range labels {
		id, err := catalog.InternLabel(txn, name)
		if err != nil {
			return err
		}
		if existing[id] {
			continue
		}
		existing[id] = true
		added = append(added, id)
		node.Labels = append(node.Labels, id)
	}
	if len(added) == 0 {
		return nil
	}
	for _, id := range added {
		if err := txn.Set(codec.LabelKey(id, nodeID), nil); err != nil {
			return err
		}
		for k, v := range node.Props {
			if !isIndexable(v) {
				continue
			}
			has, err := catalog.HasIndex(txn, id, k)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			pk, err := codec.PropKey(id, k, v, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Set(pk, nil); err != nil {
				return err
			}
		}
	}
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// RemoveLabels removes labels from a node, deleting the corresponding `l` and
// `p` index entries. Labels that are not present are ignored.
func RemoveLabels(txn storage.Txn, nodeID uint64, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	toRemove := make(map[uint32]bool)
	for _, name := range labels {
		id, found, err := catalog.LookupLabel(txn, name)
		if err != nil {
			return err
		}
		if found {
			toRemove[id] = true
		}
	}
	if len(toRemove) == 0 {
		return nil
	}
	var kept []uint32
	for _, id := range node.Labels {
		if toRemove[id] {
			if err := txn.Delete(codec.LabelKey(id, nodeID)); err != nil {
				return err
			}
			for k, v := range node.Props {
				if !isIndexable(v) {
					continue
				}
				has, err := catalog.HasIndex(txn, id, k)
				if err != nil {
					return err
				}
				if !has {
					continue
				}
				pk, err := codec.PropKey(id, k, v, nodeID)
				if err != nil {
					return err
				}
				if err := txn.Delete(pk); err != nil {
					return err
				}
			}
			continue
		}
		kept = append(kept, id)
	}
	node.Labels = kept
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// SetNodeProperties applies a map of new properties to a node. When replace is
// true the resulting node has *exactly* the provided properties (others are
// deleted); when false the new properties are merged onto the existing ones.
// Index entries are kept in sync inside the same transaction.
func SetNodeProperties(txn storage.Txn, nodeID uint64, props map[string]any, replace bool) error {
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	desired := make(map[uint32]any, len(props))
	for k, v := range props {
		keyID, err := catalog.InternKey(txn, k)
		if err != nil {
			return err
		}
		desired[keyID] = normalize(v)
	}

	if replace {
		// Delete `p` entries for properties that will be removed or changed.
		for keyID, old := range node.Props {
			newVal, kept := desired[keyID]
			if kept && scalarEqual(newVal, old) {
				continue
			}
			for _, l := range node.Labels {
				has, err := catalog.HasIndex(txn, l, keyID)
				if err != nil {
					return err
				}
				if !has || !isIndexable(old) {
					continue
				}
				pk, err := codec.PropKey(l, keyID, old, nodeID)
				if err != nil {
					return err
				}
				if err := txn.Delete(pk); err != nil {
					return err
				}
			}
		}
		node.Props = make(map[uint32]any, len(desired))
		for k, v := range desired {
			if v == nil {
				continue
			}
			node.Props[k] = v
		}
	} else {
		for keyID, newVal := range desired {
			if old, had := node.Props[keyID]; had && !scalarEqual(old, newVal) {
				for _, l := range node.Labels {
					has, err := catalog.HasIndex(txn, l, keyID)
					if err != nil {
						return err
					}
					if !has || !isIndexable(old) {
						continue
					}
					pk, err := codec.PropKey(l, keyID, old, nodeID)
					if err != nil {
						return err
					}
					if err := txn.Delete(pk); err != nil {
						return err
					}
				}
			}
			if newVal == nil {
				delete(node.Props, keyID)
			} else {
				node.Props[keyID] = newVal
			}
		}
	}

	// Write `p` entries for current properties (idempotent on existing keys).
	for keyID, v := range node.Props {
		if !isIndexable(v) {
			continue
		}
		for _, l := range node.Labels {
			has, err := catalog.HasIndex(txn, l, keyID)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			pk, err := codec.PropKey(l, keyID, v, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Set(pk, nil); err != nil {
				return err
			}
		}
	}
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// DetachDeleteNode removes all incident edges (in any direction, including
// self-loops) and then the node itself. It is the DETACH DELETE semantics.
func DetachDeleteNode(txn storage.Txn, id uint64) error {
	if _, err := GetNode(txn, id); err != nil {
		return err
	}
	edgeIDs := map[uint64]struct{}{}
	out, err := OutEdges(txn, id, 0)
	if err != nil {
		return err
	}
	for _, e := range out {
		edgeIDs[e.ID] = struct{}{}
	}
	in, err := InEdges(txn, id, 0)
	if err != nil {
		return err
	}
	for _, e := range in {
		edgeIDs[e.ID] = struct{}{}
	}
	for eid := range edgeIDs {
		if err := DeleteEdge(txn, eid); err != nil {
			return err
		}
	}
	return DeleteNode(txn, id)
}

// DeleteNode removes a node and all its index entries (`l`, `p`). Returns
// ErrNodeHasEdges if incident edges exist: the caller must remove them first
// (use DetachDeleteNode for the DETACH DELETE semantics).
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
