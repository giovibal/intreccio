package graph

import (
	"fmt"
	"math"
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
	// segs, for a property range scan: one iterator per value band, all opened
	// when the cursor is created so the scan sees one consistent state.
	segs []rangeSegment
	seg  int
}

// rangeSegment walks one band of a property index until stop reports that
// the values have passed the range's upper end.
type rangeSegment struct {
	it   storage.Iterator
	stop func(v any) bool
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
	if c.segs != nil {
		return c.nextInRange()
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

// Close releases the underlying iterators. It is safe to call more than once.
func (c *NodeCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var first error
	if c.it != nil {
		first = c.it.Close()
	}
	for _, sg := range c.segs {
		if err := sg.it.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (c *NodeCursor) nextInRange() (Node, bool, error) {
	for c.seg < len(c.segs) {
		sg := c.segs[c.seg]
		if !sg.it.Valid() {
			c.seg++
			continue
		}
		v, id, err := codec.ParsePropKey(sg.it.Key())
		if err != nil {
			return Node{}, false, err
		}
		if sg.stop != nil && sg.stop(v) {
			c.seg++
			continue
		}
		sg.it.Next()
		node, err := GetNode(c.txn, id)
		if err != nil {
			return Node{}, false, err
		}
		return node, true, nil
	}
	return Node{}, false, c.Close()
}

// PropRange describes the values a property range scan must cover. Bounds are
// scalars as produced by expression evaluation; an absent bound is open.
type PropRange struct {
	Lower, Upper                   any
	HasLower, HasUpper             bool
	LowerInclusive, UpperInclusive bool
	Prefix                         string // STARTS WITH, when HasPrefix
	HasPrefix                      bool
}

// ScanPropertyRange returns a cursor over the nodes whose indexed (label,
// propKey) value may satisfy r. The result is a superset: it covers every
// value the query comparison accepts (integers and floats compare as
// numbers), and the caller keeps the predicate as a filter to drop the rest.
// It returns nil when no value can satisfy r (a null or NaN bound, or bounds
// of different kinds).
func ScanPropertyRange(txn storage.Txn, labelID, keyID uint32, r PropRange) (*NodeCursor, error) {
	plans, ok, err := rangeBands(labelID, keyID, r)
	if err != nil || !ok {
		return nil, err
	}
	c := &NodeCursor{txn: txn, segs: make([]rangeSegment, 0, len(plans))}
	for _, p := range plans {
		c.segs = append(c.segs, rangeSegment{it: txn.ScanFrom(p.prefix, p.start), stop: p.stop})
	}
	return c, nil
}

type bandPlan struct {
	prefix, start []byte
	stop          func(any) bool
}

// rangeBands maps a range to the index bands (and the seek position and stop
// condition in each) that hold every value it may accept.
func rangeBands(labelID, keyID uint32, r PropRange) ([]bandPlan, bool, error) {
	if r.HasPrefix {
		p := codec.AppendStringPrefix(codec.PropBandPrefix(labelID, keyID, codec.BandString)[:9], r.Prefix)
		return []bandPlan{{prefix: p, start: p}}, true, nil
	}
	lo, hi := normalize(r.Lower), normalize(r.Upper)
	kind := ""
	for _, b := range []struct {
		v   any
		has bool
	}{{lo, r.HasLower}, {hi, r.HasUpper}} {
		if !b.has {
			continue
		}
		k := valueKind(b.v)
		if k == "" || (kind != "" && k != kind) {
			return nil, false, nil // null, NaN, a non-scalar or mixed kinds: nothing matches
		}
		kind = k
	}
	band := func(tag byte) []byte { return codec.PropBandPrefix(labelID, keyID, tag) }
	switch kind {
	case "string":
		p := band(codec.BandString)
		plan := bandPlan{prefix: p, start: p}
		if r.HasLower {
			start, err := codec.PropPrefix(labelID, keyID, lo)
			if err != nil {
				return nil, false, err
			}
			plan.start = start
		}
		if r.HasUpper {
			u := hi.(string)
			plan.stop = func(v any) bool { s, ok := v.(string); return !ok || s > u }
		}
		return []bandPlan{plan}, true, nil
	case "bool":
		p := band(codec.BandBool)
		return []bandPlan{{prefix: p, start: p}}, true, nil
	default: // numbers: scan the int and the float band
		ib, err := intBand(band(codec.BandInt), labelID, keyID, r, lo, hi)
		if err != nil {
			return nil, false, err
		}
		fb, err := floatBand(band(codec.BandFloat), labelID, keyID, r, lo, hi)
		if err != nil {
			return nil, false, err
		}
		return []bandPlan{ib, fb}, true, nil
	}
}

// valueKind classifies a bound: "number", "string", "bool", or "" when no
// value can compare with it (null, NaN, lists, maps...).
func valueKind(v any) string {
	switch x := v.(type) {
	case int64:
		return "number"
	case float64:
		if math.IsNaN(x) {
			return ""
		}
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	default:
		return ""
	}
}

// intBand covers the integers a numeric range may accept. Below 2^53 an
// integer and a float compare exactly, so the bounds are exact (an exclusive
// bound skips the bound value itself); beyond that the side is left open.
func intBand(prefix []byte, labelID, keyID uint32, r PropRange, lo, hi any) (bandPlan, error) {
	plan := bandPlan{prefix: prefix, start: prefix}
	if r.HasLower {
		if l, ok := intLowerBound(lo, r.LowerInclusive); ok {
			start, err := codec.PropPrefix(labelID, keyID, l)
			if err != nil {
				return plan, err
			}
			plan.start = start
		}
	}
	if r.HasUpper {
		if u, ok := intUpperBound(hi, r.UpperInclusive); ok {
			plan.stop = func(v any) bool { i, isInt := v.(int64); return !isInt || i > u }
		}
	}
	return plan, nil
}

// intLowerBound is the smallest integer that can satisfy `x > b` (or >=).
func intLowerBound(b any, inclusive bool) (int64, bool) {
	switch x := b.(type) {
	case int64:
		if inclusive {
			return x, true
		}
		if x == math.MaxInt64 {
			return 0, false
		}
		return x + 1, true
	case float64:
		if math.IsInf(x, 0) || math.Abs(x) >= maxExactInt {
			return 0, false
		}
		if inclusive {
			return int64(math.Ceil(x)), true
		}
		return int64(math.Floor(x)) + 1, true
	}
	return 0, false
}

// intUpperBound is the largest integer that can satisfy `x < b` (or <=).
func intUpperBound(b any, inclusive bool) (int64, bool) {
	switch x := b.(type) {
	case int64:
		if inclusive {
			return x, true
		}
		if x == math.MinInt64 {
			return 0, false
		}
		return x - 1, true
	case float64:
		if math.IsInf(x, 0) || math.Abs(x) >= maxExactInt {
			return 0, false
		}
		if inclusive {
			return int64(math.Floor(x)), true
		}
		return int64(math.Ceil(x)) - 1, true
	}
	return 0, false
}

// floatBand covers the floats a numeric range may accept. Float bounds, and
// integer bounds below 2^53, are exact (-0.0 sorts just before 0.0, so a
// bound of zero starts at -0.0); larger integer bounds are widened by one ulp
// because their float64 conversion may round.
func floatBand(prefix []byte, labelID, keyID uint32, r PropRange, lo, hi any) (bandPlan, error) {
	plan := bandPlan{prefix: prefix, start: prefix}
	if r.HasLower {
		if l, exact, ok := floatBound(lo); ok && !math.IsInf(l, -1) {
			switch {
			case !exact:
				l = math.Nextafter(l, math.Inf(-1))
			case !r.LowerInclusive:
				l = math.Nextafter(l, math.Inf(1)) // > 0 skips both zeros
			case l == 0:
				l = math.Copysign(0, -1) // >= 0 includes -0.0
			}
			start, err := codec.PropPrefix(labelID, keyID, l)
			if err != nil {
				return plan, err
			}
			plan.start = start
		}
	}
	if r.HasUpper {
		if u, exact, ok := floatBound(hi); ok && !math.IsInf(u, 1) {
			inclusive := r.UpperInclusive
			if !exact {
				u, inclusive = math.Nextafter(u, math.Inf(1)), true
			}
			plan.stop = func(v any) bool {
				f, isFloat := v.(float64)
				if !isFloat {
					return true
				}
				if inclusive {
					return f > u
				}
				return f >= u
			}
		}
	}
	return plan, nil
}

// floatBound converts a numeric bound to float64 and reports whether the
// conversion is exact.
func floatBound(v any) (f float64, exact, ok bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), x > -maxExactInt && x < maxExactInt, true
	case float64:
		return x, true, true
	}
	return 0, false, false
}
