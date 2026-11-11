package exec

import (
	"fmt"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/plan"
)

// shortestPathOp emits, for each input row, one row per shortest path found
// between the bound From and To nodes (at most one unless All). Rows whose
// endpoints are null produce nothing.
type shortestPathOp struct {
	ctx     *Context
	input   op
	from    string
	to      string
	pathVar string
	relVar  string
	spec    graph.PathSpec

	cur     binding
	pending []graph.Path
	pidx    int
}

func buildShortestPath(x *plan.ShortestPath, ctx *Context, arg *argumentOp) (op, error) {
	in, err := buildWith(x.Input, ctx, arg)
	if err != nil {
		return nil, err
	}
	spec := graph.PathSpec{
		Out:     x.Dir == ast.DirOut || x.Dir == ast.DirBoth,
		In:      x.Dir == ast.DirIn || x.Dir == ast.DirBoth,
		MinHops: x.MinHops,
		MaxHops: x.MaxHops,
		All:     x.All,
		Check:   ctx.check,
	}
	if spec.MinHops < 0 {
		spec.MinHops = 1
	}
	if len(x.Types) > 0 {
		spec.Types = []uint32{} // non-nil: types unknown to the database match nothing
		seen := map[uint32]bool{}
		for _, name := range x.Types {
			id, found, err := catalog.LookupType(ctx.Txn, name)
			if err != nil {
				return nil, err
			}
			if found && !seen[id] {
				seen[id] = true
				spec.Types = append(spec.Types, id)
			}
		}
	}
	return &shortestPathOp{
		ctx: ctx, input: in, from: x.From, to: x.To,
		pathVar: x.PathVar, relVar: x.RelVar, spec: spec,
	}, nil
}

func (s *shortestPathOp) next() (binding, bool, error) {
	for {
		if err := s.ctx.check(); err != nil {
			return nil, false, err
		}
		if s.pidx < len(s.pending) {
			p, err := s.load(s.pending[s.pidx])
			if err != nil {
				return nil, false, err
			}
			s.pidx++
			out := s.cur.clone()
			if s.pathVar != "" {
				out[s.pathVar] = p
			}
			if s.relVar != "" {
				out[s.relVar] = p.Rels
			}
			return out, true, nil
		}
		b, ok, err := s.input.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		a, b2 := b[s.from], b[s.to]
		if a == nil || b2 == nil {
			continue
		}
		an, ok1 := a.(graph.Node)
		bn, ok2 := b2.(graph.Node)
		if !ok1 || !ok2 {
			return nil, false, fmt.Errorf("exec: shortestPath endpoints must be nodes, got %T and %T", a, b2)
		}
		paths, err := graph.ShortestPaths(s.ctx.Txn, an.ID, bn.ID, s.spec)
		if err != nil {
			return nil, false, err
		}
		s.cur, s.pending, s.pidx = b, paths, 0
	}
}

// load reads the node and relationship records of a path found by ID.
func (s *shortestPathOp) load(p graph.Path) (graph.Path, error) {
	out := graph.Path{Nodes: make([]graph.Node, len(p.Nodes)), Rels: make([]graph.Edge, len(p.Rels))}
	for i, n := range p.Nodes {
		node, err := graph.GetNode(s.ctx.Txn, n.ID)
		if err != nil {
			return graph.Path{}, err
		}
		out.Nodes[i] = node
	}
	for i, r := range p.Rels {
		e, err := graph.GetEdge(s.ctx.Txn, r.ID)
		if err != nil {
			return graph.Path{}, err
		}
		out.Rels[i] = e
	}
	return out, nil
}

// fnNodes returns the nodes of a path, in order.
func fnNodes(_ *Context, args []any) (any, error) {
	if err := needArgs("nodes", args, 1, 1); err != nil {
		return nil, err
	}
	switch p := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Path:
		out := make([]any, len(p.Nodes))
		for i, n := range p.Nodes {
			out[i] = n
		}
		return out, nil
	default:
		return nil, fmt.Errorf("exec: nodes() expects a path, got %T", args[0])
	}
}

// fnRelationships returns the relationships of a path, in order.
func fnRelationships(_ *Context, args []any) (any, error) {
	if err := needArgs("relationships", args, 1, 1); err != nil {
		return nil, err
	}
	switch p := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Path:
		out := make([]any, len(p.Rels))
		for i, r := range p.Rels {
			out[i] = r
		}
		return out, nil
	default:
		return nil, fmt.Errorf("exec: relationships() expects a path, got %T", args[0])
	}
}

// pathIDs renders a path as its alternating node and relationship IDs, for
// equality and grouping.
func pathIDs(p graph.Path) string {
	s := fmt.Sprintf("N%d", p.Nodes[0].ID)
	for i, r := range p.Rels {
		s += fmt.Sprintf(",E%d,N%d", r.ID, p.Nodes[i+1].ID)
	}
	return s
}
