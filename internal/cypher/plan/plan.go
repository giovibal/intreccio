package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// Catalog provides the planner with information about the available indexes. In
// production it is backed by the transaction (it resolves names and queries the
// registry); tests use a fake. See ADR 0004.
type Catalog interface {
	HasIndex(label, propKey string) bool
}

// Plan builds the physical plan for the query (read path; writing is Phase 7).
func Plan(q *ast.Query, cat Catalog) (Op, error) {
	first, columns, err := planArm(q.Clauses, cat)
	if err != nil {
		return nil, err
	}
	if len(q.Unions) == 0 {
		return first, nil
	}
	parts := []Op{first}
	all := true // UNION ALL implies no dedup; if any arm uses non-ALL we dedup.
	for _, u := range q.Unions {
		armPlan, _, err := planArm(u.Clauses, cat)
		if err != nil {
			return nil, err
		}
		parts = append(parts, armPlan)
		if !u.All {
			all = false
		}
	}
	return &Union{Parts: parts, Columns: columns, All: all}, nil
}

// planArm builds the plan for a single UNION arm (or for a query without UNION)
// and returns the output column names captured from the arm's projection.
func planArm(clauses []ast.Clause, cat Catalog) (Op, []string, error) {
	pl := &planner{cat: cat, bound: map[string]bool{}}
	for _, c := range clauses {
		if err := pl.clause(c); err != nil {
			return nil, nil, err
		}
	}
	if pl.plan == nil {
		return nil, nil, fmt.Errorf("plan: no plan produced")
	}
	return pl.plan, append([]string(nil), pl.boundOrder...), nil
}

type planner struct {
	cat        Catalog
	bound      map[string]bool
	boundOrder []string
	plan       Op
	synth      int
}

func (pl *planner) bind(name string) {
	if name == "" || pl.bound[name] {
		return
	}
	pl.bound[name] = true
	pl.boundOrder = append(pl.boundOrder, name)
}

func (pl *planner) isBound(name string) bool { return name != "" && pl.bound[name] }

func (pl *planner) newName(prefix string) string {
	n := fmt.Sprintf("%s%d", prefix, pl.synth)
	pl.synth++
	return n
}

func (pl *planner) clause(c ast.Clause) error {
	switch cl := c.(type) {
	case *ast.Match:
		return pl.planMatch(cl)
	case *ast.With:
		return pl.planWith(cl)
	case *ast.Return:
		return pl.planReturn(cl)
	case *ast.Create:
		return pl.planCreate(cl)
	case *ast.Merge:
		return pl.planMerge(cl)
	case *ast.Set:
		pl.plan = &SetItems{Input: pl.plan, Items: cl.Items}
		return nil
	case *ast.Remove:
		pl.plan = &Remove{Input: pl.plan, Items: cl.Items}
		return nil
	case *ast.Unwind:
		pl.plan = &Unwind{Input: pl.plan, Expr: cl.Expr, Alias: cl.Alias}
		pl.bind(cl.Alias)
		return nil
	case *ast.Delete:
		pl.plan = &Delete{Input: pl.plan, Exprs: cl.Exprs, Detach: cl.Detach}
		return nil
	case *ast.CreateIndex:
		pl.plan = &CreateIndex{Label: cl.Label, Property: cl.Property}
		return nil
	default:
		return fmt.Errorf("plan: unsupported clause")
	}
}

// --- Write clauses ---

func (pl *planner) planCreate(c *ast.Create) error {
	pl.plan = &Create{Input: pl.plan, Parts: c.Parts}
	for _, part := range c.Parts {
		pl.bindPatternVars(part)
	}
	return nil
}

func (pl *planner) planMerge(m *ast.Merge) error {
	pl.plan = &Merge{Input: pl.plan, Part: m.Part, OnCreate: m.OnCreate, OnMatch: m.OnMatch}
	pl.bindPatternVars(m.Part)
	return nil
}

func (pl *planner) bindPatternVars(part ast.PatternPart) {
	if part.Variable != "" {
		pl.bind(part.Variable)
	}
	if part.Start.Variable != "" {
		pl.bind(part.Start.Variable)
	}
	for _, ch := range part.Chain {
		if ch.Rel.Variable != "" {
			pl.bind(ch.Rel.Variable)
		}
		if ch.Node.Variable != "" {
			pl.bind(ch.Node.Variable)
		}
	}
}

// --- Pattern matching ---

type pnode struct {
	name           string
	labels         []string
	props          map[string]ast.Expr
	reachedByScan  bool
	scanLabel      string
	consumedInline string // inline property key consumed by the anchor
}

type prel struct {
	name   string
	types  []string
	dir    ast.Direction
	props  map[string]ast.Expr
	varLen bool
	min    int
	max    int
}

type equality struct {
	variable string
	key      string
	value    ast.Expr
	conjIdx  int // index in the WHERE conjuncts, -1 if inline
}

func (pl *planner) planMatch(m *ast.Match) error {
	if m.Optional {
		return pl.planOptionalMatch(m)
	}
	var whereConj []ast.Expr
	if m.Where != nil {
		whereConj = conjuncts(m.Where)
	}
	consumed := map[int]bool{}
	whereEqs := map[string][]equality{}
	for i, e := range whereConj {
		if eq, ok := asEquality(e); ok {
			eq.conjIdx = i
			whereEqs[eq.variable] = append(whereEqs[eq.variable], eq)
		}
	}

	var residual []ast.Expr
	for _, part := range m.Parts {
		r, err := pl.planPart(part, whereEqs, consumed)
		if err != nil {
			return err
		}
		residual = append(residual, r...)
	}
	for i, e := range whereConj {
		if !consumed[i] {
			residual = append(residual, e)
		}
	}
	if len(residual) > 0 {
		pl.plan = &Filter{Input: pl.plan, Pred: andAll(residual)}
	}
	return nil
}

// planOptionalMatch builds an OuterApply: the optional pattern is planned as a
// subplan whose leaf is an Argument (replaced by the current outer row at exec
// time); the wrapper emits one row per outer with null bindings for any pattern
// variable that did not appear before this clause if the inner produced no rows.
func (pl *planner) planOptionalMatch(m *ast.Match) error {
	outer := pl.plan
	boundBefore := make(map[string]bool, len(pl.bound))
	for k, v := range pl.bound {
		boundBefore[k] = v
	}

	// Plan the pattern starting from an Argument leaf.
	pl.plan = &Argument{}

	// Same per-MATCH logic as planMatch, but accumulating onto the subplan.
	var whereConj []ast.Expr
	if m.Where != nil {
		whereConj = conjuncts(m.Where)
	}
	consumed := map[int]bool{}
	whereEqs := map[string][]equality{}
	for i, e := range whereConj {
		if eq, ok := asEquality(e); ok {
			eq.conjIdx = i
			whereEqs[eq.variable] = append(whereEqs[eq.variable], eq)
		}
	}
	var residual []ast.Expr
	for _, part := range m.Parts {
		r, err := pl.planPart(part, whereEqs, consumed)
		if err != nil {
			return err
		}
		residual = append(residual, r...)
	}
	for i, e := range whereConj {
		if !consumed[i] {
			residual = append(residual, e)
		}
	}
	if len(residual) > 0 {
		pl.plan = &Filter{Input: pl.plan, Pred: andAll(residual)}
	}

	inner := pl.plan

	// Variables introduced by this clause are those in pl.bound but not in
	// boundBefore. Preserve binding order for stability.
	var newVars []string
	for _, name := range pl.boundOrder {
		if !boundBefore[name] {
			newVars = append(newVars, name)
		}
	}

	pl.plan = &OuterApply{Outer: outer, Inner: inner, NewVars: newVars}
	return nil
}

func (pl *planner) planPart(part ast.PatternPart, whereEqs map[string][]equality, consumed map[int]bool) ([]ast.Expr, error) {
	nodes, rels := pl.buildPart(part)

	startIdx := -1
	for i := range nodes {
		if pl.isBound(nodes[i].name) {
			startIdx = i
			break
		}
	}

	if startIdx == -1 {
		idx, scan, eq := pl.chooseAnchor(nodes, whereEqs)
		if pl.plan == nil {
			pl.plan = scan
		} else {
			pl.plan = &CartesianProduct{Left: pl.plan, Right: scan}
		}
		nodes[idx].reachedByScan = true
		nodes[idx].scanLabel = scanLabelOf(scan)
		if eq != nil {
			if eq.conjIdx >= 0 {
				consumed[eq.conjIdx] = true
			} else {
				nodes[idx].consumedInline = eq.key
			}
		}
		pl.bind(nodes[idx].name)
		startIdx = idx
	}

	// Expand rightward and leftward starting from the anchor.
	for i := startIdx; i < len(rels); i++ {
		pl.emitExpand(nodes[i], rels[i], nodes[i+1], false)
	}
	for i := startIdx - 1; i >= 0; i-- {
		pl.emitExpand(nodes[i+1], rels[i], nodes[i], true)
	}

	return residualPredicates(nodes, rels), nil
}

func (pl *planner) emitExpand(from pnode, rel prel, to pnode, reversed bool) {
	dir := rel.dir
	if reversed {
		dir = flip(dir)
	}
	pl.plan = &Expand{
		Input: pl.plan, From: from.name, Rel: rel.name, To: to.name,
		Types: rel.types, Dir: dir,
		VarLength: rel.varLen, MinHops: rel.min, MaxHops: rel.max,
		ToBound: pl.isBound(to.name),
	}
	pl.bind(rel.name)
	pl.bind(to.name)
}

func (pl *planner) chooseAnchor(nodes []pnode, whereEqs map[string][]equality) (int, Op, *equality) {
	bestIdx, bestScore := 0, -1
	var bestScan Op
	var bestEq *equality
	for i := range nodes {
		if pl.isBound(nodes[i].name) {
			continue
		}
		score, scan, eq := pl.scoreNode(nodes[i], whereEqs[nodes[i].name])
		if score > bestScore {
			bestIdx, bestScore, bestScan, bestEq = i, score, scan, eq
		}
	}
	if bestScan == nil { // all nodes already bound: fallback (should not happen here)
		bestScan = &AllNodesScan{Var: nodes[0].name}
	}
	return bestIdx, bestScan, bestEq
}

func (pl *planner) scoreNode(nd pnode, whereEqs []equality) (int, Op, *equality) {
	for _, label := range nd.labels {
		for _, k := range sortedKeys(nd.props) {
			if pl.cat != nil && pl.cat.HasIndex(label, k) {
				eq := &equality{variable: nd.name, key: k, value: nd.props[k], conjIdx: -1}
				return 3, &NodeByProperty{Var: nd.name, Label: label, Key: k, Value: nd.props[k]}, eq
			}
		}
		for _, eq := range whereEqs {
			if pl.cat != nil && pl.cat.HasIndex(label, eq.key) {
				e := eq
				return 3, &NodeByProperty{Var: nd.name, Label: label, Key: eq.key, Value: eq.value}, &e
			}
		}
	}
	if len(nd.labels) > 0 {
		return 2, &NodeByLabelScan{Var: nd.name, Label: nd.labels[0]}, nil
	}
	return 1, &AllNodesScan{Var: nd.name}, nil
}

func (pl *planner) buildPart(part ast.PatternPart) ([]pnode, []prel) {
	nodes := []pnode{pl.makeNode(part.Start)}
	var rels []prel
	for _, ch := range part.Chain {
		rels = append(rels, pl.makeRel(ch.Rel))
		nodes = append(nodes, pl.makeNode(ch.Node))
	}
	return nodes, rels
}

func (pl *planner) makeNode(n *ast.NodePattern) pnode {
	name := n.Variable
	if name == "" {
		name = pl.newName("_n")
	}
	return pnode{name: name, labels: n.Labels, props: n.Props}
}

func (pl *planner) makeRel(r *ast.RelPattern) prel {
	name := r.Variable
	if name == "" {
		name = pl.newName("_r")
	}
	return prel{name: name, types: r.Types, dir: r.Direction, props: r.Props, varLen: r.VarLength, min: r.MinHops, max: r.MaxHops}
}

// residualPredicates collects the predicates to apply in a Filter after
// scan/expand: labels not guaranteed by the access method, inline properties
// (excluding those consumed by the anchor) and relationship properties.
func residualPredicates(nodes []pnode, rels []prel) []ast.Expr {
	var out []ast.Expr
	for _, nd := range nodes {
		enforced := ""
		if nd.reachedByScan {
			enforced = nd.scanLabel
		}
		var rest []string
		for _, l := range nd.labels {
			if l != enforced {
				rest = append(rest, l)
			}
		}
		if len(rest) > 0 {
			out = append(out, &ast.LabelsPredicate{Expr: &ast.Variable{Name: nd.name}, Labels: rest})
		}
		for _, k := range sortedKeys(nd.props) {
			if k == nd.consumedInline {
				continue
			}
			out = append(out, eqExpr(nd.name, k, nd.props[k]))
		}
	}
	for _, r := range rels {
		for _, k := range sortedKeys(r.props) {
			out = append(out, eqExpr(r.name, k, r.props[k]))
		}
	}
	return out
}

// --- Projection ---

func (pl *planner) planReturn(r *ast.Return) error {
	pl.projectOrAggregate(r.Star, r.Items, r.Distinct)
	pl.applyTail(r.OrderBy, r.Skip, r.Limit)
	return nil
}

func (pl *planner) planWith(w *ast.With) error {
	pl.projectOrAggregate(w.Star, w.Items, w.Distinct)
	if w.Where != nil {
		pl.plan = &Filter{Input: pl.plan, Pred: w.Where}
	}
	pl.applyTail(w.OrderBy, w.Skip, w.Limit)
	return nil
}

func (pl *planner) projectOrAggregate(star bool, items []ast.ReturnItem, distinct bool) {
	projItems := pl.expandItems(star, items)

	var groups, aggs []ProjItem
	hasAgg := false
	for _, pi := range projItems {
		if containsAggregate(pi.Expr) {
			aggs = append(aggs, pi)
			hasAgg = true
		} else {
			groups = append(groups, pi)
		}
	}
	if hasAgg {
		pl.plan = &Aggregate{Input: pl.plan, GroupKeys: groups, Aggs: aggs, Distinct: distinct}
	} else {
		pl.plan = &Project{Input: pl.plan, Items: projItems, Distinct: distinct}
	}

	// Reset the scope to the projected columns (WITH/RETURN semantics).
	pl.bound = map[string]bool{}
	pl.boundOrder = nil
	for _, pi := range projItems {
		pl.bind(pi.Column)
	}
}

func (pl *planner) expandItems(star bool, items []ast.ReturnItem) []ProjItem {
	var out []ProjItem
	if star {
		for _, v := range pl.boundOrder {
			out = append(out, ProjItem{Expr: &ast.Variable{Name: v}, Column: v})
		}
	}
	for _, it := range items {
		col := it.Alias
		if col == "" {
			col = exprString(it.Expr)
		}
		out = append(out, ProjItem{Expr: it.Expr, Column: col})
	}
	return out
}

func (pl *planner) applyTail(orderBy []ast.SortItem, skip, limit ast.Expr) {
	if len(orderBy) > 0 {
		keys := make([]SortKey, len(orderBy))
		for i, s := range orderBy {
			keys[i] = SortKey{Expr: s.Expr, Desc: s.Desc}
		}
		pl.plan = &Sort{Input: pl.plan, Keys: keys}
	}
	if skip != nil {
		pl.plan = &Skip{Input: pl.plan, Count: skip}
	}
	if limit != nil {
		pl.plan = &Limit{Input: pl.plan, Count: limit}
	}
}

// --- Expression helpers ---

func conjuncts(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.Binary); ok && b.Op == "AND" {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	return []ast.Expr{e}
}

func andAll(exprs []ast.Expr) ast.Expr {
	out := exprs[0]
	for _, e := range exprs[1:] {
		out = &ast.Binary{Op: "AND", Left: out, Right: e}
	}
	return out
}

func asEquality(e ast.Expr) (equality, bool) {
	b, ok := e.(*ast.Binary)
	if !ok || b.Op != "=" {
		return equality{}, false
	}
	if v, k, val, ok := propConst(b.Left, b.Right); ok {
		return equality{variable: v, key: k, value: val, conjIdx: -1}, true
	}
	if v, k, val, ok := propConst(b.Right, b.Left); ok {
		return equality{variable: v, key: k, value: val, conjIdx: -1}, true
	}
	return equality{}, false
}

func propConst(a, b ast.Expr) (string, string, ast.Expr, bool) {
	pa, ok := a.(*ast.PropertyAccess)
	if !ok {
		return "", "", nil, false
	}
	v, ok := pa.Target.(*ast.Variable)
	if !ok || !isConst(b) {
		return "", "", nil, false
	}
	return v.Name, pa.Key, b, true
}

func isConst(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Literal, *ast.Param:
		return true
	default:
		return false
	}
}

func eqExpr(v, k string, val ast.Expr) ast.Expr {
	return &ast.Binary{Op: "=", Left: &ast.PropertyAccess{Target: &ast.Variable{Name: v}, Key: k}, Right: val}
}

func flip(d ast.Direction) ast.Direction {
	switch d {
	case ast.DirOut:
		return ast.DirIn
	case ast.DirIn:
		return ast.DirOut
	default:
		return ast.DirBoth
	}
}

func scanLabelOf(o Op) string {
	switch s := o.(type) {
	case *NodeByLabelScan:
		return s.Label
	case *NodeByProperty:
		return s.Label
	default:
		return ""
	}
}

func containsAggregate(e ast.Expr) bool {
	switch ex := e.(type) {
	case *ast.FunctionCall:
		if isAggregate(ex.Name) {
			return true
		}
		for _, a := range ex.Args {
			if containsAggregate(a) {
				return true
			}
		}
		return false
	case *ast.Binary:
		return containsAggregate(ex.Left) || containsAggregate(ex.Right)
	case *ast.Unary:
		return containsAggregate(ex.Expr)
	case *ast.PropertyAccess:
		return containsAggregate(ex.Target)
	case *ast.LabelsPredicate:
		return containsAggregate(ex.Expr)
	default:
		return false
	}
}

var aggregates = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true, "collect": true,
}

func isAggregate(name string) bool { return aggregates[strings.ToLower(name)] }

func sortedKeys(m map[string]ast.Expr) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
