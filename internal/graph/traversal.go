package graph

import (
	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/storage"
	"github.com/giovibal/mycypher/internal/storage/codec"
)

// NodesByLabel returns the IDs of the nodes with the given label, in nodeID order
// (prefix scan on `l`).
func NodesByLabel(txn storage.Txn, labelID uint32) ([]uint64, error) {
	var out []uint64
	it := txn.Scan(codec.LabelPrefix(labelID))
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		_, node, err := codec.ParseLabelKey(it.Key())
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

// OutEdges returns the outgoing edges from nodeID. If typeID is 0 it considers all
// types, otherwise only the given one (prefix scan on `o`).
func OutEdges(txn storage.Txn, nodeID uint64, typeID uint32) ([]EdgeRef, error) {
	prefix := codec.OutPrefixAll(nodeID)
	if typeID != 0 {
		prefix = codec.OutPrefix(nodeID, typeID)
	}
	var out []EdgeRef
	it := txn.Scan(prefix)
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		src, typ, dst, edge, err := codec.ParseOutKey(it.Key())
		if err != nil {
			return nil, err
		}
		out = append(out, EdgeRef{ID: edge, Type: typ, Src: src, Dst: dst})
	}
	return out, nil
}

// InEdges returns the incoming edges into nodeID. If typeID is 0 it considers all
// types (prefix scan on `i`).
func InEdges(txn storage.Txn, nodeID uint64, typeID uint32) ([]EdgeRef, error) {
	prefix := codec.InPrefixAll(nodeID)
	if typeID != 0 {
		prefix = codec.InPrefix(nodeID, typeID)
	}
	var out []EdgeRef
	it := txn.Scan(prefix)
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		dst, typ, src, edge, err := codec.ParseInKey(it.Key())
		if err != nil {
			return nil, err
		}
		out = append(out, EdgeRef{ID: edge, Type: typ, Src: src, Dst: dst})
	}
	return out, nil
}

// NodesByProperty returns the IDs of the nodes with the given label and property
// value. It uses the `p` index if present for (label, propKey), otherwise it falls
// back to a label scan with a filter on the record.
func NodesByProperty(txn storage.Txn, labelID, keyID uint32, value any) ([]uint64, error) {
	value = normalize(value)

	has, err := catalog.HasIndex(txn, labelID, keyID)
	if err != nil {
		return nil, err
	}
	if has && isIndexable(value) {
		prefix, err := codec.PropPrefix(labelID, keyID, value)
		if err != nil {
			return nil, err
		}
		var out []uint64
		it := txn.Scan(prefix)
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			node, err := codec.ParsePropKeyNode(it.Key())
			if err != nil {
				return nil, err
			}
			out = append(out, node)
		}
		return out, nil
	}

	// Fallback without index: label scan + filter on the record.
	nodes, err := NodesByLabel(txn, labelID)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, n := range nodes {
		node, err := GetNode(txn, n)
		if err != nil {
			return nil, err
		}
		if v, ok := node.Props[keyID]; ok && scalarEqual(v, value) {
			out = append(out, n)
		}
	}
	return out, nil
}
