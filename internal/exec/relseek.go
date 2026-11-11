package exec

import (
	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/plan"
)

// relSeek emits a row per (relationship, orientation) found by a
// relationship property seek; see plan.RelByProperty.
type relSeek struct {
	ctx  *Context
	x    *plan.RelByProperty
	ids  []uint64
	pos  int
	rows []binding // pending rows of the current relationship
}

func buildRelSeek(x *plan.RelByProperty, ctx *Context, arg *argumentOp) (op, error) {
	s := &relSeek{ctx: ctx, x: x}
	typeID, found, err := catalog.LookupType(ctx.Txn, x.Type)
	if err != nil || !found {
		return s, err
	}
	keyID, found, err := catalog.LookupKey(ctx.Txn, x.Key)
	if err != nil || !found {
		return s, err
	}
	row := binding{}
	if arg != nil {
		row = arg.row // inside an Apply: the value may use the outer row
	}
	value, err := eval(x.Value, row, ctx)
	if err != nil {
		return nil, err
	}
	value = normalize(value)
	switch {
	case value == nil:
		// = null is never true
	case !graph.IsIndexable(value):
		// Lists and maps have no index keys: compare every relationship of
		// the type with the query equality, as the non-indexed path does.
		err = graph.ScanEdgesOfType(ctx.Txn, typeID, func(e graph.Edge) error {
			if v, ok := e.Props[keyID]; ok && equals(v, value) {
				s.ids = append(s.ids, e.ID)
			}
			return nil
		})
	default:
		s.ids, err = graph.EdgesByProperty(ctx.Txn, typeID, keyID, value)
	}
	return s, err
}

func (s *relSeek) next() (binding, bool, error) {
	for {
		if err := s.ctx.check(); err != nil {
			return nil, false, err
		}
		if len(s.rows) > 0 {
			r := s.rows[0]
			s.rows = s.rows[1:]
			return r, true, nil
		}
		if s.pos >= len(s.ids) {
			return nil, false, nil
		}
		id := s.ids[s.pos]
		s.pos++
		e, err := graph.GetEdge(s.ctx.Txn, id)
		if err != nil {
			return nil, false, err
		}
		src, err := graph.GetNode(s.ctx.Txn, e.Src)
		if err != nil {
			return nil, false, err
		}
		dst := src
		if e.Dst != e.Src {
			if dst, err = graph.GetNode(s.ctx.Txn, e.Dst); err != nil {
				return nil, false, err
			}
		}
		// Orientations as (from, to), in the order an expand would produce
		// them: outgoing first.
		var pairs [][2]graph.Node
		if s.x.Dir == ast.DirOut || s.x.Dir == ast.DirBoth {
			pairs = append(pairs, [2]graph.Node{src, dst})
		}
		if s.x.Dir == ast.DirIn || s.x.Dir == ast.DirBoth {
			pairs = append(pairs, [2]graph.Node{dst, src})
		}
		for _, p := range pairs {
			if s.x.From == s.x.To && p[0].ID != p[1].ID {
				continue // (a)-[r]-(a): self-loops only
			}
			s.rows = append(s.rows, binding{s.x.Rel: e, s.x.From: p[0], s.x.To: p[1]})
		}
	}
}
