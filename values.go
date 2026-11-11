package intreccio

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
)

// Node is a node in a query result: its ID, labels and properties, as of
// the query's transaction. It can be passed back as a parameter
// (MATCH (n) WHERE n = $node), which refers to the stored node by ID.
type Node struct {
	ID     int64
	Labels []string
	Props  map[string]any
}

// Relationship is a relationship in a query result. StartID and EndID are
// the IDs of its start and end nodes.
type Relationship struct {
	ID      int64
	Type    string
	StartID int64
	EndID   int64
	Props   map[string]any
}

// Path is a path in a query result (from shortestPath): Relationships[i]
// joins Nodes[i] and Nodes[i+1].
type Path struct {
	Nodes         []Node
	Relationships []Relationship
}

// String renders the node in Cypher syntax, e.g. (:Person {name: "Ann"}).
func (n Node) String() string {
	var b strings.Builder
	b.WriteByte('(')
	for _, l := range n.Labels {
		b.WriteString(":" + l)
	}
	writeProps(&b, n.Props, len(n.Labels) > 0)
	b.WriteByte(')')
	return b.String()
}

// String renders the relationship in Cypher syntax, e.g. [:KNOWS {since: 2020}].
func (r Relationship) String() string {
	var b strings.Builder
	b.WriteString("[:" + r.Type)
	writeProps(&b, r.Props, true)
	b.WriteByte(']')
	return b.String()
}

// String renders the path in Cypher syntax, with each relationship pointing
// the way it is stored.
func (p Path) String() string {
	var b strings.Builder
	for i, n := range p.Nodes {
		b.WriteString(n.String())
		if i == len(p.Relationships) {
			break
		}
		r := p.Relationships[i]
		if r.StartID == n.ID {
			b.WriteString("-" + r.String() + "->")
		} else {
			b.WriteString("<-" + r.String() + "-")
		}
	}
	return b.String()
}

func writeProps(b *strings.Builder, props map[string]any, space bool) {
	if len(props) == 0 {
		return
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if space {
		b.WriteByte(' ')
	}
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k + ": ")
		if s, ok := props[k].(string); ok {
			b.WriteString(strconv.Quote(s))
		} else {
			fmt.Fprint(b, props[k])
		}
	}
	b.WriteByte('}')
}

// publicRows converts the engine's values in rows into the public types,
// resolving label, type and property names in txn. Names are cached for the
// rows of one query.
func publicRows(txn storage.Txn, rows [][]any) error {
	r := &resolver{txn: txn, names: map[nameKey]string{}}
	for _, row := range rows {
		for i, v := range row {
			pv, err := r.public(v)
			if err != nil {
				return err
			}
			row[i] = pv
		}
	}
	return nil
}

type nameKey struct {
	kind byte // 'L' label, 'T' type, 'K' property key
	id   uint32
}

type resolver struct {
	txn   storage.Txn
	names map[nameKey]string
}

func (r *resolver) name(kind byte, id uint32) (string, error) {
	k := nameKey{kind, id}
	if n, ok := r.names[k]; ok {
		return n, nil
	}
	var (
		n   string
		err error
	)
	switch kind {
	case 'L':
		n, err = catalog.LabelName(r.txn, id)
	case 'T':
		n, err = catalog.TypeName(r.txn, id)
	default:
		n, err = catalog.KeyName(r.txn, id)
	}
	if err != nil {
		return "", fmt.Errorf("resolve name %c%d: %w", kind, id, err)
	}
	r.names[k] = n
	return n, nil
}

// public converts one value, recursing into lists and maps.
func (r *resolver) public(v any) (any, error) {
	switch x := v.(type) {
	case graph.Node:
		return r.node(x)
	case graph.Edge:
		return r.rel(x)
	case []graph.Edge: // a variable-length relationship
		out := make([]any, len(x))
		for i, e := range x {
			rel, err := r.rel(e)
			if err != nil {
				return nil, err
			}
			out[i] = rel
		}
		return out, nil
	case graph.Path:
		p := Path{Nodes: make([]Node, len(x.Nodes)), Relationships: make([]Relationship, len(x.Rels))}
		for i, n := range x.Nodes {
			pn, err := r.node(n)
			if err != nil {
				return nil, err
			}
			p.Nodes[i] = pn
		}
		for i, e := range x.Rels {
			rel, err := r.rel(e)
			if err != nil {
				return nil, err
			}
			p.Relationships[i] = rel
		}
		return p, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			pv, err := r.public(e)
			if err != nil {
				return nil, err
			}
			out[i] = pv
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			pv, err := r.public(e)
			if err != nil {
				return nil, err
			}
			out[k] = pv
		}
		return out, nil
	}
	return v, nil
}

func (r *resolver) node(n graph.Node) (Node, error) {
	out := Node{ID: int64(n.ID), Labels: make([]string, len(n.Labels))}
	for i, id := range n.Labels {
		l, err := r.name('L', id)
		if err != nil {
			return Node{}, err
		}
		out.Labels[i] = l
	}
	props, err := r.props(n.Props)
	out.Props = props
	return out, err
}

func (r *resolver) rel(e graph.Edge) (Relationship, error) {
	typ, err := r.name('T', e.Type)
	if err != nil {
		return Relationship{}, err
	}
	props, err := r.props(e.Props)
	return Relationship{ID: int64(e.ID), Type: typ, StartID: int64(e.Src), EndID: int64(e.Dst), Props: props}, err
}

func (r *resolver) props(p map[uint32]any) (map[string]any, error) {
	out := make(map[string]any, len(p))
	for id, v := range p {
		k, err := r.name('K', id)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// engineParams replaces the nodes, relationships and paths among the
// parameters with the stored elements they name, read in txn. An element
// that no longer exists becomes null.
func engineParams(txn storage.Txn, params map[string]any) (map[string]any, error) {
	var out map[string]any
	for k, v := range params {
		ev, changed, err := engineValue(txn, v)
		if err != nil {
			return nil, fmt.Errorf("query: parameter %q: %w", k, err)
		}
		if !changed {
			continue
		}
		if out == nil { // copy on first change: the caller's map stays as is
			out = make(map[string]any, len(params))
			for k2, v2 := range params {
				out[k2] = v2
			}
		}
		out[k] = ev
	}
	if out == nil {
		return params, nil
	}
	return out, nil
}

func engineValue(txn storage.Txn, v any) (any, bool, error) {
	switch x := v.(type) {
	case Node:
		n, err := graph.GetNode(txn, uint64(x.ID))
		if errors.Is(err, graph.ErrNodeNotFound) {
			return nil, true, nil
		}
		return n, true, err
	case Relationship:
		e, err := graph.GetEdge(txn, uint64(x.ID))
		if errors.Is(err, graph.ErrEdgeNotFound) {
			return nil, true, nil
		}
		return e, true, err
	case Path:
		p := graph.Path{}
		for _, n := range x.Nodes {
			gn, err := graph.GetNode(txn, uint64(n.ID))
			if errors.Is(err, graph.ErrNodeNotFound) {
				return nil, true, nil
			} else if err != nil {
				return nil, true, err
			}
			p.Nodes = append(p.Nodes, gn)
		}
		for _, r := range x.Relationships {
			ge, err := graph.GetEdge(txn, uint64(r.ID))
			if errors.Is(err, graph.ErrEdgeNotFound) {
				return nil, true, nil
			} else if err != nil {
				return nil, true, err
			}
			p.Rels = append(p.Rels, ge)
		}
		if len(p.Nodes) == 0 {
			return nil, true, nil
		}
		return p, true, nil
	case []any:
		var out []any
		for i, e := range x {
			ev, changed, err := engineValue(txn, e)
			if err != nil {
				return nil, false, err
			}
			if changed && out == nil {
				out = append([]any(nil), x...)
			}
			if out != nil {
				out[i] = ev
			}
		}
		if out == nil {
			return v, false, nil
		}
		return out, true, nil
	case map[string]any:
		var out map[string]any
		for k, e := range x {
			ev, changed, err := engineValue(txn, e)
			if err != nil {
				return nil, false, err
			}
			if changed && out == nil {
				out = make(map[string]any, len(x))
				for k2, v2 := range x {
					out[k2] = v2
				}
			}
			if out != nil {
				out[k] = ev
			}
		}
		if out == nil {
			return v, false, nil
		}
		return out, true, nil
	}
	return v, false, nil
}
