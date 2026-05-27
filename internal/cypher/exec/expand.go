package exec

import (
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/graph"
)

func buildExpandWith(x *plan.Expand, ctx *Context, arg *argumentOp) (op, error) {
	if x.VarLength {
		return buildVarExpandWith(x, ctx, arg)
	}
	in, err := buildWith(x.Input, ctx, arg)
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

// varExpandMaxDepth bounds the BFS when the user-specified upper hop count is
// unbounded (MaxHops = -1), to avoid runaway traversal in pathological graphs.
const varExpandMaxDepth = 100

// varExpand implements variable-length expansion with trail semantics (no
// repeated relationship along a path). For each input row, a depth-bounded
// BFS from the From node yields every path within [MinHops, MaxHops]; each
// path produces one output row. Edges are reused across paths but not within
// a single path.
type varExpand struct {
	ctx     *Context
	input   op
	from    string
	rel     string
	to      string
	dir     ast.Direction
	anyType bool
	typeIDs []uint32
	toBound bool
	minHops int
	maxHops int

	cur     binding
	pending []varHit
	pidx    int
}

type varHit struct {
	toID  uint64
	edges []uint64
}

func buildVarExpandWith(x *plan.Expand, ctx *Context, arg *argumentOp) (op, error) {
	in, err := buildWith(x.Input, ctx, arg)
	if err != nil {
		return nil, err
	}
	e := &varExpand{
		ctx: ctx, input: in,
		from: x.From, rel: x.Rel, to: x.To,
		dir: x.Dir, toBound: x.ToBound,
		anyType: len(x.Types) == 0,
		minHops: x.MinHops, maxHops: x.MaxHops,
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

func (e *varExpand) next() (binding, bool, error) {
	for {
		if e.pidx >= len(e.pending) {
			b, ok, err := e.input.next()
			if err != nil || !ok {
				return nil, ok, err
			}
			fromNode, ok := b[e.from].(graph.Node)
			if !ok {
				return nil, false, fmt.Errorf("exec: var-length expand source %s is not a node", e.from)
			}
			hits, err := e.bfs(fromNode.ID)
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
			if ok && existing.ID != hit.toID {
				continue
			}
		}

		toNode, err := graph.GetNode(e.ctx.Txn, hit.toID)
		if err != nil {
			return nil, false, err
		}
		edges, err := e.fetchEdges(hit.edges)
		if err != nil {
			return nil, false, err
		}
		out := e.cur.clone()
		out[e.to] = toNode
		out[e.rel] = edges
		return out, true, nil
	}
}

// bfs enumerates every trail (no repeated relationship) starting at fromID
// whose length is within [minHops, maxHops]. With MaxHops unbounded the depth
// is capped at varExpandMaxDepth.
func (e *varExpand) bfs(fromID uint64) ([]varHit, error) {
	minH := e.minHops
	if minH < 0 {
		minH = 1
	}
	maxH := e.maxHops
	if maxH < 0 {
		maxH = varExpandMaxDepth
	}

	var hits []varHit
	if minH <= 0 && 0 <= maxH {
		hits = append(hits, varHit{toID: fromID})
	}
	if maxH == 0 {
		return hits, nil
	}

	type frontElem struct {
		nodeID uint64
		edges  []uint64
	}
	frontier := []frontElem{{nodeID: fromID}}

	for d := 1; d <= maxH; d++ {
		var next []frontElem
		for _, f := range frontier {
			neighbors, err := e.neighbors(f.nodeID)
			if err != nil {
				return nil, err
			}
			for _, nb := range neighbors {
				if containsEdge(f.edges, nb.edgeID) {
					continue
				}
				newEdges := make([]uint64, len(f.edges)+1)
				copy(newEdges, f.edges)
				newEdges[len(f.edges)] = nb.edgeID

				next = append(next, frontElem{nodeID: nb.otherID, edges: newEdges})
				if d >= minH {
					hits = append(hits, varHit{toID: nb.otherID, edges: newEdges})
				}
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	return hits, nil
}

type neighborHit struct {
	edgeID  uint64
	otherID uint64
}

func (e *varExpand) neighbors(nodeID uint64) ([]neighborHit, error) {
	var out []neighborHit
	addOut := e.dir == ast.DirOut || e.dir == ast.DirBoth
	addIn := e.dir == ast.DirIn || e.dir == ast.DirBoth

	types := e.typeIDs
	if e.anyType {
		types = []uint32{0}
	}
	if len(types) == 0 {
		return nil, nil
	}

	if addOut {
		for _, t := range types {
			refs, err := graph.OutEdges(e.ctx.Txn, nodeID, t)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				out = append(out, neighborHit{edgeID: r.ID, otherID: r.Dst})
			}
		}
	}
	if addIn {
		for _, t := range types {
			refs, err := graph.InEdges(e.ctx.Txn, nodeID, t)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				out = append(out, neighborHit{edgeID: r.ID, otherID: r.Src})
			}
		}
	}
	return out, nil
}

func (e *varExpand) fetchEdges(ids []uint64) ([]graph.Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]graph.Edge, len(ids))
	for i, id := range ids {
		edge, err := graph.GetEdge(e.ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		out[i] = edge
	}
	return out, nil
}

func containsEdge(ids []uint64, id uint64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
