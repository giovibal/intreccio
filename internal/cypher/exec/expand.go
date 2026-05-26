package exec

import (
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/graph"
)

func buildExpand(x *plan.Expand, ctx *Context) (op, error) {
	if x.VarLength {
		return nil, fmt.Errorf("exec: variable-length expand not yet supported (Phase 8)")
	}
	in, err := build(x.Input, ctx)
	if err != nil {
		return nil, err
	}
	e := &expand{
		ctx:     ctx,
		input:   in,
		from:    x.From,
		rel:     x.Rel,
		to:      x.To,
		dir:     x.Dir,
		toBound: x.ToBound,
		anyType: len(x.Types) == 0,
	}
	for _, name := range x.Types {
		id, found, err := catalog.LookupType(ctx.Txn, name)
		if err != nil {
			return nil, err
		}
		if found {
			e.typeIDs = append(e.typeIDs, id)
		}
	}
	return e, nil
}

// expand follows the edges of the From node and produces the To node (and the
// relationship binding) for each matching edge.
type expand struct {
	ctx     *Context
	input   op
	from    string
	rel     string
	to      string
	dir     ast.Direction
	toBound bool
	anyType bool
	typeIDs []uint32

	cur     binding
	pending []edgeHit
	pidx    int
}

// edgeHit is a matched edge plus the reached endpoint.
type edgeHit struct {
	ref   graph.EdgeRef
	other uint64
}

func (e *expand) next() (binding, bool, error) {
	for {
		if e.pidx >= len(e.pending) {
			b, ok, err := e.input.next()
			if err != nil || !ok {
				return nil, ok, err
			}
			fromNode, ok := b[e.from].(graph.Node)
			if !ok {
				return nil, false, fmt.Errorf("exec: expand source %s is not a node", e.from)
			}
			hits, err := e.gather(fromNode.ID)
			if err != nil {
				return nil, false, err
			}
			e.cur = b
			e.pending = hits
			e.pidx = 0
			continue
		}

		hit := e.pending[e.pidx]
		e.pidx++

		if e.toBound {
			existing, ok := e.cur[e.to].(graph.Node)
			if ok && existing.ID != hit.other {
				continue
			}
		}

		toNode, err := graph.GetNode(e.ctx.Txn, hit.other)
		if err != nil {
			return nil, false, err
		}
		edge, err := graph.GetEdge(e.ctx.Txn, hit.ref.ID)
		if err != nil {
			return nil, false, err
		}

		out := e.cur.clone()
		out[e.to] = toNode
		out[e.rel] = edge
		return out, true, nil
	}
}

// gather collects the candidate edges from nodeID according to direction and types.
func (e *expand) gather(nodeID uint64) ([]edgeHit, error) {
	var hits []edgeHit
	addOut := e.dir == ast.DirOut || e.dir == ast.DirBoth
	addIn := e.dir == ast.DirIn || e.dir == ast.DirBoth

	if addOut {
		for _, typeID := range e.typeFilter() {
			refs, err := graph.OutEdges(e.ctx.Txn, nodeID, typeID)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				hits = append(hits, edgeHit{ref: r, other: r.Dst})
			}
		}
	}
	if addIn {
		for _, typeID := range e.typeFilter() {
			refs, err := graph.InEdges(e.ctx.Txn, nodeID, typeID)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				hits = append(hits, edgeHit{ref: r, other: r.Src})
			}
		}
	}
	return hits, nil
}

// typeFilter returns the type IDs to scan: {0} (all) when no types were specified,
// otherwise the resolved type IDs (possibly empty if none exist).
func (e *expand) typeFilter() []uint32 {
	if e.anyType {
		return []uint32{0}
	}
	return e.typeIDs
}
