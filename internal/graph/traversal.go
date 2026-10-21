package graph

import (
	"fmt"
	"sort"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// AllNodes returns the IDs of all nodes (prefix scan on `n`). It is the fallback
// access method; prefer NodesByLabel/NodesByProperty where possible.
func AllNodes(txn storage.Txn) ([]uint64, error) {
	var out []uint64
	it := txn.Scan(codec.NodePrefix())
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		id, err := codec.NodeIDFromKey(it.Key())
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

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
	probes, exact := equalityProbes(value)
	if has && exact && isIndexable(value) {
		var out []uint64
		for _, p := range probes {
			prefix, err := codec.PropPrefix(labelID, keyID, p)
			if err != nil {
				return nil, err
			}
			if out, err = appendIndexMatches(txn, out, prefix); err != nil {
				return nil, err
			}
		}
		// Several probes visit different key ranges: restore node-ID order.
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
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
		if v, ok := node.Props[keyID]; ok && QueryEqual(v, value) {
			out = append(out, n)
		}
	}
	return out, nil
}

// appendIndexMatches appends the node IDs of the `p` entries under prefix.
func appendIndexMatches(txn storage.Txn, out []uint64, prefix []byte) ([]uint64, error) {
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

// NodeCursor streams the nodes of a scan one at a time, so a caller that stops
// early (LIMIT, an error, a cancelled query) reads only what it consumed. It
// holds one storage iterator for its whole life, which also fixes the set of
// nodes it returns: writes made later in the same transaction are not seen, as
// with the ID lists it replaces. The caller must Close it before the
// transaction ends; Close is idempotent and Next closes it when exhausted.
type NodeCursor struct {
	txn     storage.Txn
	it      storage.Iterator
	byLabel bool
	closed  bool
}

// ScanAllNodes returns a cursor over every node, in nodeID order. Records are
// decoded straight from the `n` keyspace, without a lookup per node.
func ScanAllNodes(txn storage.Txn) *NodeCursor {
	return &NodeCursor{txn: txn, it: txn.Scan(codec.NodePrefix())}
}

// ScanLabel returns a cursor over the nodes with the given label, in nodeID
// order (`l` keyspace, then one record lookup per node).
func ScanLabel(txn storage.Txn, labelID uint32) *NodeCursor {
	return &NodeCursor{txn: txn, it: txn.Scan(codec.LabelPrefix(labelID)), byLabel: true}
}

// Next returns the next node; ok is false once the scan is exhausted.
func (c *NodeCursor) Next() (node Node, ok bool, err error) {
	if c.closed {
		return Node{}, false, nil
	}
	if !c.it.Valid() {
		return Node{}, false, c.Close()
	}
	key := c.it.Key()
	if c.byLabel {
		_, id, err := codec.ParseLabelKey(key)
		if err != nil {
			return Node{}, false, err
		}
		c.it.Next()
		node, err := GetNode(c.txn, id)
		if err != nil {
			return Node{}, false, err
		}
		return node, true, nil
	}
	id, err := codec.NodeIDFromKey(key)
	if err != nil {
		return Node{}, false, err
	}
	val, err := c.it.Value()
	if err != nil {
		return Node{}, false, err
	}
	c.it.Next()
	rec, err := codec.DecodeNode(val)
	if err != nil {
		return Node{}, false, fmt.Errorf("graph: decode node %d: %w", id, err)
	}
	return Node{ID: id, NodeRecord: rec}, true, nil
}

// Closed reports whether the cursor has been closed (explicitly or because it
// was exhausted).
func (c *NodeCursor) Closed() bool { return c.closed }

// Close releases the underlying iterator. It is safe to call more than once.
func (c *NodeCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.it.Close()
}
