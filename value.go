package intreccio

import (
	"sort"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
)

// Node is a graph node as it appears in a query result (for example the value
// of n in `MATCH (n) RETURN n`). ID is the same identifier id(n) returns.
// Labels are sorted; Props is never nil.
type Node struct {
	ID     int64
	Labels []string
	Props  map[string]any
}

// Relationship is a graph relationship as it appears in a query result. ID is
// the same identifier id(r) returns; Start and End are the IDs of the nodes it
// connects, in the direction it was created. Props is never nil.
type Relationship struct {
	ID    int64
	Type  string
	Start int64
	End   int64
	Props map[string]any
}

// exporter turns the executor's internal values into the public result model:
// graph.Node becomes Node and graph.Edge becomes Relationship, with dictionary
// IDs resolved to names. It must run while the query's transaction is open. The
// per-query name caches keep a large result from re-reading the dictionaries
// for every row.
type exporter struct {
	txn    storage.Txn
	labels map[uint32]string
	types  map[uint32]string
	keys   map[uint32]string
}

func newExporter(txn storage.Txn) *exporter {
	return &exporter{
		txn:    txn,
		labels: map[uint32]string{},
		types:  map[uint32]string{},
		keys:   map[uint32]string{},
	}
}

// rows exports every value of rows in place. The row slices are owned by the
// executor's result, so replacing their elements is safe; nested lists and maps
// are copied before being changed because they may belong to the caller (a
// parameter returned as-is).
func (e *exporter) rows(rows [][]any) error {
	for _, row := range rows {
		for i, v := range row {
			out, _, err := e.value(v)
			if err != nil {
				return err
			}
			row[i] = out
		}
	}
	return nil
}

// value exports v, reporting whether the result differs from the input.
func (e *exporter) value(v any) (any, bool, error) {
	switch x := v.(type) {
	case graph.Node:
		n, err := e.node(x)
		return n, true, err
	case graph.Edge:
		r, err := e.relationship(x)
		return r, true, err
	case []graph.Edge: // the relationships of a variable-length match
		out := make([]any, len(x))
		for i, edge := range x {
			r, err := e.relationship(edge)
			if err != nil {
				return nil, false, err
			}
			out[i] = r
		}
		return out, true, nil
	case []any:
		var out []any // allocated on the first changed element
		for i, el := range x {
			ev, changed, err := e.value(el)
			if err != nil {
				return nil, false, err
			}
			if changed && out == nil {
				out = make([]any, len(x))
				copy(out, x[:i])
			}
			if out != nil {
				out[i] = ev
			}
		}
		if out == nil {
			return x, false, nil
		}
		return out, true, nil
	case map[string]any:
		var out map[string]any // allocated on the first changed entry
		for k, el := range x {
			ev, changed, err := e.value(el)
			if err != nil {
				return nil, false, err
			}
			if !changed {
				continue
			}
			if out == nil {
				out = make(map[string]any, len(x))
				for k2, v2 := range x {
					out[k2] = v2
				}
			}
			out[k] = ev
		}
		if out == nil {
			return x, false, nil
		}
		return out, true, nil
	default:
		return v, false, nil
	}
}

func (e *exporter) node(n graph.Node) (Node, error) {
	labels := make([]string, len(n.Labels))
	for i, id := range n.Labels {
		name, err := e.name(e.labels, catalog.LabelName, id)
		if err != nil {
			return Node{}, err
		}
		labels[i] = name
	}
	sort.Strings(labels)
	props, err := e.props(n.Props)
	if err != nil {
		return Node{}, err
	}
	return Node{ID: int64(n.ID), Labels: labels, Props: props}, nil
}

func (e *exporter) relationship(r graph.Edge) (Relationship, error) {
	typ, err := e.name(e.types, catalog.TypeName, r.Type)
	if err != nil {
		return Relationship{}, err
	}
	props, err := e.props(r.Props)
	if err != nil {
		return Relationship{}, err
	}
	return Relationship{ID: int64(r.ID), Type: typ, Start: int64(r.Src), End: int64(r.Dst), Props: props}, nil
}

// props resolves the property-key IDs of a record. Property values are scalars,
// lists or maps and never contain graph entities, so they are kept as they are.
func (e *exporter) props(in map[uint32]any) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for id, v := range in {
		name, err := e.name(e.keys, catalog.KeyName, id)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

func (e *exporter) name(cache map[uint32]string, resolve func(storage.Txn, uint32) (string, error), id uint32) (string, error) {
	if name, ok := cache[id]; ok {
		return name, nil
	}
	name, err := resolve(e.txn, id)
	if err != nil {
		return "", err
	}
	cache[id] = name
	return name, nil
}
