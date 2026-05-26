package exec

import (
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/graph"
	"github.com/giovibal/mycypher/internal/storage/codec"
)

// --- CREATE ---

type createOp struct {
	ctx   *Context
	input op
	parts []ast.PatternPart
}

func (c *createOp) next() (binding, bool, error) {
	b, ok, err := c.input.next()
	if err != nil || !ok {
		return nil, ok, err
	}
	out := b.clone()
	for _, part := range c.parts {
		if err := c.createPart(out, part); err != nil {
			return nil, false, err
		}
	}
	return out, true, nil
}

func (c *createOp) createPart(b binding, part ast.PatternPart) error {
	prev, err := createOrReuseNode(c.ctx, b, part.Start)
	if err != nil {
		return err
	}
	for _, ch := range part.Chain {
		next, err := createOrReuseNode(c.ctx, b, ch.Node)
		if err != nil {
			return err
		}
		if err := createRel(c.ctx, b, ch.Rel, prev, next); err != nil {
			return err
		}
		prev = next
	}
	return nil
}

func createOrReuseNode(ctx *Context, b binding, n *ast.NodePattern) (graph.Node, error) {
	if n.Variable != "" {
		if existing, ok := b[n.Variable].(graph.Node); ok {
			return existing, nil
		}
	}
	propMap, err := evalProps(n.Props, b, ctx)
	if err != nil {
		return graph.Node{}, err
	}
	id, err := graph.CreateNode(ctx.Txn, n.Labels, propMap)
	if err != nil {
		return graph.Node{}, err
	}
	node, err := graph.GetNode(ctx.Txn, id)
	if err != nil {
		return graph.Node{}, err
	}
	if n.Variable != "" {
		b[n.Variable] = node
	}
	return node, nil
}

func createRel(ctx *Context, b binding, r *ast.RelPattern, from, to graph.Node) error {
	if r.Direction == ast.DirBoth {
		return fmt.Errorf("exec: CREATE requires a directed relationship")
	}
	if len(r.Types) != 1 {
		return fmt.Errorf("exec: CREATE requires exactly one relationship type")
	}
	if r.VarLength {
		return fmt.Errorf("exec: CREATE does not support variable-length relationships")
	}
	if r.Variable != "" {
		if _, ok := b[r.Variable]; ok {
			return fmt.Errorf("exec: relationship variable %s is already bound", r.Variable)
		}
	}
	propMap, err := evalProps(r.Props, b, ctx)
	if err != nil {
		return err
	}
	src, dst := from.ID, to.ID
	if r.Direction == ast.DirIn {
		src, dst = dst, src
	}
	id, err := graph.CreateEdge(ctx.Txn, r.Types[0], src, dst, propMap)
	if err != nil {
		return err
	}
	edge, err := graph.GetEdge(ctx.Txn, id)
	if err != nil {
		return err
	}
	if r.Variable != "" {
		b[r.Variable] = edge
	}
	return nil
}

func evalProps(props map[string]ast.Expr, b binding, ctx *Context) (map[string]any, error) {
	if len(props) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(props))
	for k, v := range props {
		val, err := eval(v, b, ctx)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

// --- SET ---

type setOp struct {
	ctx   *Context
	input op
	items []ast.SetItem
}

func (s *setOp) next() (binding, bool, error) {
	b, ok, err := s.input.next()
	if err != nil || !ok {
		return nil, ok, err
	}
	for _, item := range s.items {
		if item.Target == nil {
			return nil, false, fmt.Errorf("exec: SET target must be a property access")
		}
		varRef, ok := item.Target.Target.(*ast.Variable)
		if !ok {
			return nil, false, fmt.Errorf("exec: SET target must be a variable's property")
		}
		node, ok := b[varRef.Name].(graph.Node)
		if !ok {
			return nil, false, fmt.Errorf("exec: SET target %s is not a node", varRef.Name)
		}
		val, err := eval(item.Value, b, s.ctx)
		if err != nil {
			return nil, false, err
		}
		if err := graph.SetProperty(s.ctx.Txn, node.ID, item.Target.Key, val); err != nil {
			return nil, false, err
		}
		refreshed, err := graph.GetNode(s.ctx.Txn, node.ID)
		if err != nil {
			return nil, false, err
		}
		b[varRef.Name] = refreshed
	}
	return b, true, nil
}

// --- DELETE / DETACH DELETE ---

type deleteOp struct {
	ctx    *Context
	input  op
	exprs  []ast.Expr
	detach bool
}

func (d *deleteOp) next() (binding, bool, error) {
	b, ok, err := d.input.next()
	if err != nil || !ok {
		return nil, ok, err
	}
	for _, e := range d.exprs {
		v, err := eval(e, b, d.ctx)
		if err != nil {
			return nil, false, err
		}
		switch t := v.(type) {
		case nil:
			continue
		case graph.Edge:
			if err := graph.DeleteEdge(d.ctx.Txn, t.ID); err != nil {
				return nil, false, err
			}
		case graph.Node:
			if d.detach {
				if err := graph.DetachDeleteNode(d.ctx.Txn, t.ID); err != nil {
					return nil, false, err
				}
			} else {
				if err := graph.DeleteNode(d.ctx.Txn, t.ID); err != nil {
					return nil, false, err
				}
			}
		default:
			return nil, false, fmt.Errorf("exec: DELETE expects a node or relationship, got %T", v)
		}
	}
	return b, true, nil
}

// --- CREATE INDEX ---

type createIndexOp struct {
	ctx   *Context
	label string
	prop  string
	done  bool
}

func (c *createIndexOp) next() (binding, bool, error) {
	if c.done {
		return nil, false, nil
	}
	c.done = true

	labelID, err := catalog.InternLabel(c.ctx.Txn, c.label)
	if err != nil {
		return nil, false, err
	}
	keyID, err := catalog.InternKey(c.ctx.Txn, c.prop)
	if err != nil {
		return nil, false, err
	}

	has, err := catalog.HasIndex(c.ctx.Txn, labelID, keyID)
	if err != nil {
		return nil, false, err
	}
	if has {
		// Idempotent: registry entry already present and any prior backfill is
		// kept consistent by the graph layer on every subsequent mutation.
		return binding{}, true, nil
	}

	if err := catalog.AddIndex(c.ctx.Txn, labelID, keyID); err != nil {
		return nil, false, err
	}

	// Backfill: scan every node carrying the label and write a `p` entry for
	// each whose property value is a scalar.
	nodeIDs, err := graph.NodesByLabel(c.ctx.Txn, labelID)
	if err != nil {
		return nil, false, err
	}
	for _, id := range nodeIDs {
		node, err := graph.GetNode(c.ctx.Txn, id)
		if err != nil {
			return nil, false, err
		}
		v, ok := node.Props[keyID]
		if !ok || !graph.IsIndexable(v) {
			continue
		}
		pk, err := codec.PropKey(labelID, keyID, v, id)
		if err != nil {
			return nil, false, err
		}
		if err := c.ctx.Txn.Set(pk, nil); err != nil {
			return nil, false, err
		}
	}
	return binding{}, true, nil
}

// --- MERGE ---

type mergeOp struct {
	ctx   *Context
	input op
	part  ast.PatternPart

	out []binding
	pos int
}

func (m *mergeOp) next() (binding, bool, error) {
	for {
		if m.pos < len(m.out) {
			r := m.out[m.pos]
			m.pos++
			return r, true, nil
		}
		b, ok, err := m.input.next()
		if err != nil || !ok {
			return nil, ok, err
		}
		rows, err := m.run(b)
		if err != nil {
			return nil, false, err
		}
		m.out = rows
		m.pos = 0
	}
}

func (m *mergeOp) run(b binding) ([]binding, error) {
	switch len(m.part.Chain) {
	case 0:
		return m.mergeNode(b, m.part.Start)
	case 1:
		return m.mergeRel(b, m.part.Start, m.part.Chain[0])
	default:
		return nil, fmt.Errorf("exec: MERGE with more than one relationship is not yet supported")
	}
}

// mergeNode: match nodes with all labels and inline props; create one if none match.
func (m *mergeOp) mergeNode(b binding, n *ast.NodePattern) ([]binding, error) {
	labelIDs, allLabelsExist, err := resolveLabels(m.ctx, n.Labels)
	if err != nil {
		return nil, err
	}
	var matches []binding
	if allLabelsExist {
		var ids []uint64
		if len(labelIDs) > 0 {
			ids, err = graph.NodesByLabel(m.ctx.Txn, labelIDs[0])
		} else {
			ids, err = graph.AllNodes(m.ctx.Txn)
		}
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			node, err := graph.GetNode(m.ctx.Txn, id)
			if err != nil {
				return nil, err
			}
			if !hasAllLabelIDs(node, labelIDs) {
				continue
			}
			ok, err := propsMatch(m.ctx, node.Props, n.Props, b)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			nb := b.clone()
			if n.Variable != "" {
				nb[n.Variable] = node
			}
			matches = append(matches, nb)
		}
	}
	if len(matches) > 0 {
		return matches, nil
	}
	// No match: create.
	nb := b.clone()
	if _, err := createOrReuseNode(m.ctx, nb, n); err != nil {
		return nil, err
	}
	return []binding{nb}, nil
}

// mergeRel: a single relationship from the start node. The start must reference a
// bound variable; the end node may be bound or unbound (with labels/props).
func (m *mergeOp) mergeRel(b binding, start *ast.NodePattern, ch ast.PatternChain) ([]binding, error) {
	rel := ch.Rel
	end := ch.Node
	if rel.Direction == ast.DirBoth {
		return nil, fmt.Errorf("exec: MERGE requires a directed relationship")
	}
	if len(rel.Types) != 1 {
		return nil, fmt.Errorf("exec: MERGE requires exactly one relationship type")
	}
	if rel.VarLength {
		return nil, fmt.Errorf("exec: MERGE does not support variable-length relationships")
	}
	if start.Variable == "" {
		return nil, fmt.Errorf("exec: MERGE start node must reference a bound variable")
	}
	startNode, ok := b[start.Variable].(graph.Node)
	if !ok {
		return nil, fmt.Errorf("exec: MERGE start variable %s is not bound", start.Variable)
	}

	var endNode graph.Node
	endBound := false
	if end.Variable != "" {
		if n, ok := b[end.Variable].(graph.Node); ok {
			endNode = n
			endBound = true
		}
	}

	endLabelIDs, endLabelsExist, err := resolveLabels(m.ctx, end.Labels)
	if err != nil {
		return nil, err
	}

	typeID, found, err := catalog.LookupType(m.ctx.Txn, rel.Types[0])
	if err != nil {
		return nil, err
	}
	var matches []binding
	if found && endLabelsExist {
		var refs []graph.EdgeRef
		if rel.Direction == ast.DirOut {
			refs, err = graph.OutEdges(m.ctx.Txn, startNode.ID, typeID)
		} else {
			refs, err = graph.InEdges(m.ctx.Txn, startNode.ID, typeID)
		}
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			otherID := r.Dst
			if rel.Direction == ast.DirIn {
				otherID = r.Src
			}
			if endBound && otherID != endNode.ID {
				continue
			}
			otherNode, err := graph.GetNode(m.ctx.Txn, otherID)
			if err != nil {
				return nil, err
			}
			if !endBound {
				if !hasAllLabelIDs(otherNode, endLabelIDs) {
					continue
				}
				ok, err := propsMatch(m.ctx, otherNode.Props, end.Props, b)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			edge, err := graph.GetEdge(m.ctx.Txn, r.ID)
			if err != nil {
				return nil, err
			}
			ok, err := propsMatch(m.ctx, edge.Props, rel.Props, b)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			nb := b.clone()
			if rel.Variable != "" {
				nb[rel.Variable] = edge
			}
			if end.Variable != "" {
				nb[end.Variable] = otherNode
			}
			matches = append(matches, nb)
		}
	}
	if len(matches) > 0 {
		return matches, nil
	}

	// No match: create.
	nb := b.clone()
	endResolved := endNode
	if !endBound {
		propMap, err := evalProps(end.Props, b, m.ctx)
		if err != nil {
			return nil, err
		}
		id, err := graph.CreateNode(m.ctx.Txn, end.Labels, propMap)
		if err != nil {
			return nil, err
		}
		newEnd, err := graph.GetNode(m.ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		endResolved = newEnd
		if end.Variable != "" {
			nb[end.Variable] = newEnd
		}
	}
	if err := createRel(m.ctx, nb, rel, startNode, endResolved); err != nil {
		return nil, err
	}
	return []binding{nb}, nil
}

// --- shared helpers ---

func resolveLabels(ctx *Context, names []string) ([]uint32, bool, error) {
	ids := make([]uint32, 0, len(names))
	for _, name := range names {
		id, found, err := catalog.LookupLabel(ctx.Txn, name)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, nil
		}
		ids = append(ids, id)
	}
	return ids, true, nil
}

func hasAllLabelIDs(n graph.Node, ids []uint32) bool {
	for _, id := range ids {
		if !hasLabel(n, id) {
			return false
		}
	}
	return true
}

func propsMatch(ctx *Context, have map[uint32]any, want map[string]ast.Expr, b binding) (bool, error) {
	for k, expr := range want {
		expected, err := eval(expr, b, ctx)
		if err != nil {
			return false, err
		}
		keyID, found, err := catalog.LookupKey(ctx.Txn, k)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		if !scalarEqualNorm(have[keyID], expected) {
			return false, nil
		}
	}
	return true, nil
}

// scalarEqualNorm compares two scalar values for equality after normalization.
func scalarEqualNorm(a, b any) bool {
	a, b = normalize(a), normalize(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	c, ok := compareValues(a, b)
	return ok && c == 0
}
