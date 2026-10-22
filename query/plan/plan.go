package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/giovibal/intreccio/query/ast"
)

// Catalog provides the planner with information about the available indexes. In
// production it is backed by the transaction (it resolves names and queries the
// registry); tests use a fake. See ADR 0004.
type Catalog interface {
	HasIndex(label, propKey string) bool
}

// NoIndexes is a Catalog that reports no indexes. It is the convenient choice
// for callers that plan a query without a database — e.g. parsing and inspecting
// the plan of an openCypher query as a standalone library.
var NoIndexes Catalog = noIndexes{}

type noIndexes struct{}

func (noIndexes) HasIndex(_, _ string) bool { return false }

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
		pl.plan = &CreateIndex{Name: cl.Name, Label: cl.Label, Property: cl.Property}
		return nil
	case *ast.DropIndex:
		pl.plan = &DropIndex{Name: cl.Name, IfExists: cl.IfExists}
		return nil
	case *ast.CreateConstraint:
		pl.plan = &CreateConstraint{Name: cl.Name, Label: cl.Label, Property: cl.Property}
		return nil
	case *ast.DropConstraint:
		pl.plan = &DropConstraint{Name: cl.Name, IfExists: cl.IfExists}
		return nil
	case *ast.ShowConstraints:
		pl.plan = &ShowConstraints{}
		for _, c := range []string{"name", "label", "property", "type"} {
			pl.bind(c)
		}
		return nil
	case *ast.ShowIndexes:
		pl.plan = &ShowIndexes{}
		for _, c := range []string{"name", "label", "property"} {
			pl.bind(c)
		}
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
	anon           bool // no variable in the query text
	labels         []string
	props          map[string]ast.Expr
	reachedByScan  bool
	scanLabel      string
	consumedInline string // inline property key consumed by the anchor
}

type prel struct {
	name   string
	anon   bool // no variable in the query text
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
		for _, eq := range asEqualities(e) {
			eq.conjIdx = i
			whereEqs[eq.variable] = append(whereEqs[eq.variable], eq)
		}
	}
	whereRanges := map[string][]rangePred{}
	for _, e := range whereConj {
		if r, ok := asRange(e); ok {
			whereRanges[r.variable] = append(whereRanges[r.variable], r)
		}
	}

	var residual []ast.Expr
	for _, part := range shortestLast(m.Parts) {
		r, err := pl.planPart(part, whereEqs, whereRanges, consumed)
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
	if err := checkShortestFilters(m.Parts, residual); err != nil {
		return err
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
		for _, eq := range asEqualities(e) {
			eq.conjIdx = i
			whereEqs[eq.variable] = append(whereEqs[eq.variable], eq)
		}
	}
	whereRanges := map[string][]rangePred{}
	for _, e := range whereConj {
		if r, ok := asRange(e); ok {
			whereRanges[r.variable] = append(whereRanges[r.variable], r)
		}
	}
	var residual []ast.Expr
	for _, part := range shortestLast(m.Parts) {
		r, err := pl.planPart(part, whereEqs, whereRanges, consumed)
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
	if err := checkShortestFilters(m.Parts, residual); err != nil {
		return err
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

func (pl *planner) planPart(part ast.PatternPart, whereEqs map[string][]equality, whereRanges map[string][]rangePred, consumed map[int]bool) ([]ast.Expr, error) {
	if part.Shortest != ast.NotShortest {
		return nil, pl.planShortest(part)
	}
	nodes, rels := pl.buildPart(part)

	startIdx := -1
	for i := range nodes {
		if pl.isBound(nodes[i].name) {
			startIdx = i
			break
		}
	}

	if startIdx == -1 {
		idx, scan, eq := pl.chooseAnchor(nodes, whereEqs, whereRanges)
		switch {
		case pl.plan == nil:
			pl.plan = scan
		case len(scanVars(scan)) > 0:
			// The seek value depends on the incoming row: run the seek once
			// per row instead of as an independent scan.
			pl.plan = &Apply{Outer: pl.plan, Inner: &CartesianProduct{Left: &Argument{}, Right: scan}}
		default:
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
	var expands []*Expand
	for i := startIdx; i < len(rels); i++ {
		expands = append(expands, pl.emitExpand(nodes[i], rels[i], nodes[i+1], false))
	}
	for i := startIdx - 1; i >= 0; i-- {
		expands = append(expands, pl.emitExpand(nodes[i+1], rels[i], nodes[i], true))
	}

	residual := residualPredicates(nodes, rels)
	if part.Variable == "" { // a path variable observes every element
		markUnused(expands, nodes, rels, residual)
	}
	return residual, nil
}

// markUnused flags the anonymous targets and relationships of single-hop
// expands that no residual predicate reads, so the executor can skip loading
// their records. Anonymous elements cannot be named later in the query.
func markUnused(expands []*Expand, nodes []pnode, rels []prel, residual []ast.Expr) {
	read := map[string]bool{}
	for _, e := range residual {
		for _, v := range exprVars(e) {
			read[v] = true
		}
	}
	anon := map[string]bool{}
	for _, nd := range nodes {
		anon[nd.name] = nd.anon
	}
	for _, r := range rels {
		anon[r.name] = r.anon
	}
	for _, x := range expands {
		if x.VarLength {
			continue
		}
		x.ToUnused = anon[x.To] && !read[x.To] && !x.ToBound
		x.RelUnused = anon[x.Rel] && !read[x.Rel]
	}
}

func (pl *planner) emitExpand(from pnode, rel prel, to pnode, reversed bool) *Expand {
	dir := rel.dir
	if reversed {
		dir = flip(dir)
	}
	x := &Expand{
		Input: pl.plan, From: from.name, Rel: rel.name, To: to.name,
		Types: rel.types, Dir: dir,
		VarLength: rel.varLen, MinHops: rel.min, MaxHops: rel.max,
		ToBound: pl.isBound(to.name),
	}
	pl.plan = x
	pl.bind(rel.name)
	pl.bind(to.name)
	return x
}

func (pl *planner) chooseAnchor(nodes []pnode, whereEqs map[string][]equality, whereRanges map[string][]rangePred) (int, Op, *equality) {
	bestIdx, bestScore := 0, -1
	var bestScan Op
	var bestEq *equality
	for i := range nodes {
		if pl.isBound(nodes[i].name) {
			continue
		}
		score, scan, eq := pl.scoreNode(nodes[i], whereEqs[nodes[i].name], whereRanges[nodes[i].name])
		if score > bestScore {
			bestIdx, bestScore, bestScan, bestEq = i, score, scan, eq
		}
	}
	if bestScan == nil { // all nodes already bound: fallback (should not happen here)
		bestScan = &AllNodesScan{Var: nodes[0].name}
	}
	return bestIdx, bestScan, bestEq
}

// scoreNode picks the access method for a pattern node: an index seek on an
// equality (4), an index range scan (3), a label scan (2) or a full scan (1).
func (pl *planner) scoreNode(nd pnode, whereEqs []equality, ranges []rangePred) (int, Op, *equality) {
	for _, label := range nd.labels {
		for _, k := range sortedKeys(nd.props) {
			if pl.cat != nil && pl.cat.HasIndex(label, k) && pl.seekable(nd.props[k]) {
				eq := &equality{variable: nd.name, key: k, value: nd.props[k], conjIdx: -1}
				return 4, &NodeByProperty{Var: nd.name, Label: label, Key: k, Value: nd.props[k]}, eq
			}
		}
		for _, eq := range whereEqs {
			if pl.cat != nil && pl.cat.HasIndex(label, eq.key) && pl.seekable(eq.value) {
				e := eq
				return 4, &NodeByProperty{Var: nd.name, Label: label, Key: eq.key, Value: eq.value}, &e
			}
		}
	}
	for _, label := range nd.labels {
		if r := pl.rangeScan(nd.name, label, ranges); r != nil {
			return 3, r, nil
		}
	}
	if len(nd.labels) > 0 {
		return 2, &NodeByLabelScan{Var: nd.name, Label: nd.labels[0]}, nil
	}
	return 1, &AllNodesScan{Var: nd.name}, nil
}

// rangeScan builds an index range scan for the first indexed key that has a
// usable range predicate. The predicates stay in the residual filter: the scan
// only narrows the candidates.
func (pl *planner) rangeScan(variable, label string, ranges []rangePred) *NodeByPropertyRange {
	if pl.cat == nil {
		return nil
	}
	byKey := map[string][]rangePred{}
	var keys []string
	for _, r := range ranges {
		if _, seen := byKey[r.key]; !seen {
			keys = append(keys, r.key)
		}
		byKey[r.key] = append(byKey[r.key], r)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !pl.cat.HasIndex(label, k) {
			continue
		}
		scan := &NodeByPropertyRange{Var: variable, Label: label, Key: k}
		for _, r := range byKey[k] {
			if !pl.seekable(r.value) {
				continue
			}
			switch r.op {
			case "STARTS WITH":
				if scan.Prefix == nil {
					scan.Prefix = r.value
				}
			case ">", ">=":
				if scan.Lower == nil {
					scan.Lower, scan.LowerInclusive = r.value, r.op == ">="
				}
			case "<", "<=":
				if scan.Upper == nil {
					scan.Upper, scan.UpperInclusive = r.value, r.op == "<="
				}
			}
		}
		if scan.Prefix != nil {
			scan.Lower, scan.Upper = nil, nil // a prefix alone bounds the scan
		}
		if scan.Prefix != nil || scan.Lower != nil || scan.Upper != nil {
			return scan
		}
	}
	return nil
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
	name, anon := n.Variable, n.Variable == ""
	if anon {
		name = pl.newName("_n")
	}
	return pnode{name: name, anon: anon, labels: n.Labels, props: n.Props}
}

func (pl *planner) makeRel(r *ast.RelPattern) prel {
	name, anon := r.Variable, r.Variable == ""
	if anon {
		name = pl.newName("_r")
	}
	return prel{name: name, anon: anon, types: r.Types, dir: r.Direction, props: r.Props, varLen: r.VarLength, min: r.MinHops, max: r.MaxHops}
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

// asEqualities returns the `var.key = value` readings of an equality
// conjunct, one per side that is a property of a variable. Whether the value
// can drive an index seek is decided later (seekable), once it is known which
// variables are bound.
func asEqualities(e ast.Expr) []equality {
	b, ok := e.(*ast.Binary)
	if !ok || b.Op != "=" {
		return nil
	}
	var out []equality
	if v, k, ok := propOfVar(b.Left); ok {
		out = append(out, equality{variable: v, key: k, value: b.Right, conjIdx: -1})
	}
	if v, k, ok := propOfVar(b.Right); ok {
		out = append(out, equality{variable: v, key: k, value: b.Left, conjIdx: -1})
	}
	return out
}

func propOfVar(e ast.Expr) (variable, key string, ok bool) {
	pa, ok := e.(*ast.PropertyAccess)
	if !ok {
		return "", "", false
	}
	v, ok := pa.Target.(*ast.Variable)
	if !ok {
		return "", "", false
	}
	return v.Name, pa.Key, true
}

// seekable reports whether value can be evaluated before the node is found:
// every variable it uses must already be bound, and it must not aggregate.
func (pl *planner) seekable(value ast.Expr) bool {
	if containsAggregate(value) {
		return false
	}
	for _, v := range exprVars(value) {
		if !pl.isBound(v) {
			return false
		}
	}
	return true
}

// localVars returns the variables read by exprs other than the local
// variable they bind (a list comprehension or quantifier variable).
func localVars(local string, exprs ...ast.Expr) []string {
	var out []string
	for _, e := range exprs {
		for _, v := range exprVars(e) {
			if v != local {
				out = append(out, v)
			}
		}
	}
	return out
}

// exprVars returns the variable names an expression reads.
func exprVars(e ast.Expr) []string {
	var out []string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case nil, *ast.Literal, *ast.Param:
		case *ast.Variable:
			out = append(out, x.Name)
		case *ast.PropertyAccess:
			walk(x.Target)
		case *ast.Subscript:
			walk(x.Target)
			walk(x.Index)
		case *ast.Slice:
			walk(x.Target)
			walk(x.From)
			walk(x.To)
		case *ast.ListComprehension:
			walk(x.List)
			out = append(out, localVars(x.Variable, x.Where, x.Map)...)
		case *ast.Quantifier:
			walk(x.List)
			out = append(out, localVars(x.Variable, x.Where)...)
		case *ast.Unary:
			walk(x.Expr)
		case *ast.Binary:
			walk(x.Left)
			walk(x.Right)
		case *ast.FunctionCall:
			for _, a := range x.Args {
				walk(a)
			}
		case *ast.LabelsPredicate:
			walk(x.Expr)
		case *ast.ListLiteral:
			for _, el := range x.Elements {
				walk(el)
			}
		case *ast.MapLiteral:
			for _, k := range sortedKeys(x.Entries) {
				walk(x.Entries[k])
			}
		case *ast.Case:
			walk(x.Operand)
			for _, w := range x.Whens {
				walk(w.Cond)
				walk(w.Result)
			}
			walk(x.Else)
		default:
			// Unknown expression kind: report a sentinel that is never bound,
			// so the value is not used for a seek.
			out = append(out, "\x00unknown")
		}
	}
	walk(e)
	return out
}

// rangePred is `variable.key op value` with op one of >, >=, <, <=, or
// STARTS WITH (variable.key on the left).
type rangePred struct {
	variable, key, op string
	value             ast.Expr
}

// asRange reads a range conjunct, flipping `value < n.k` into `n.k > value`.
func asRange(e ast.Expr) (rangePred, bool) {
	b, ok := e.(*ast.Binary)
	if !ok {
		return rangePred{}, false
	}
	flipped := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<="}
	switch b.Op {
	case "<", "<=", ">", ">=":
		if v, k, ok := propOfVar(b.Left); ok {
			return rangePred{variable: v, key: k, op: b.Op, value: b.Right}, true
		}
		if v, k, ok := propOfVar(b.Right); ok {
			return rangePred{variable: v, key: k, op: flipped[b.Op], value: b.Left}, true
		}
	case "STARTS WITH":
		if v, k, ok := propOfVar(b.Left); ok {
			return rangePred{variable: v, key: k, op: b.Op, value: b.Right}, true
		}
	}
	return rangePred{}, false
}

// scanVars returns the variables an access method reads from the incoming
// row; a non-empty result means the scan must run once per row.
func scanVars(o Op) []string {
	switch x := o.(type) {
	case *NodeByProperty:
		return exprVars(x.Value)
	case *NodeByPropertyRange:
		var out []string
		for _, e := range []ast.Expr{x.Lower, x.Upper, x.Prefix} {
			if e != nil {
				out = append(out, exprVars(e)...)
			}
		}
		return out
	}
	return nil
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
	case *NodeByPropertyRange:
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
	case *ast.Subscript:
		return containsAggregate(ex.Target) || containsAggregate(ex.Index)
	case *ast.Slice:
		return containsAggregate(ex.Target) || containsAggregate(ex.From) || containsAggregate(ex.To)
	case *ast.ListComprehension:
		return containsAggregate(ex.List) || containsAggregate(ex.Where) || containsAggregate(ex.Map)
	case *ast.Quantifier:
		return containsAggregate(ex.List) || containsAggregate(ex.Where)
	case *ast.LabelsPredicate:
		return containsAggregate(ex.Expr)
	case *ast.ListLiteral:
		for _, el := range ex.Elements {
			if containsAggregate(el) {
				return true
			}
		}
		return false
	case *ast.MapLiteral:
		for _, v := range ex.Entries {
			if containsAggregate(v) {
				return true
			}
		}
		return false
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

// shortestLast orders the parts of a MATCH so shortest-path parts come after
// the patterns that bind their endpoints.
func shortestLast(parts []ast.PatternPart) []ast.PatternPart {
	out := make([]ast.PatternPart, 0, len(parts))
	for _, p := range parts {
		if p.Shortest == ast.NotShortest {
			out = append(out, p)
		}
	}
	for _, p := range parts {
		if p.Shortest != ast.NotShortest {
			out = append(out, p)
		}
	}
	return out
}

// planShortest adds a ShortestPath over the current plan. Both endpoints must
// be bound by now (by an earlier clause or another part of the same MATCH).
func (pl *planner) planShortest(part ast.PatternPart) error {
	from, to := part.Start.Variable, part.Chain[0].Node.Variable
	for _, v := range []string{from, to} {
		if !pl.isBound(v) {
			return fmt.Errorf("plan: shortestPath endpoint %s must be bound by an earlier pattern, e.g. MATCH (%s {..}), ..., p = shortestPath(...)", v, v)
		}
	}
	if pl.plan == nil {
		return fmt.Errorf("plan: shortestPath needs its endpoints bound first")
	}
	rel := part.Chain[0].Rel
	pl.plan = &ShortestPath{
		Input:   pl.plan,
		From:    from,
		To:      to,
		PathVar: part.Variable,
		RelVar:  rel.Variable,
		Types:   rel.Types,
		Dir:     rel.Direction,
		MinHops: rel.MinHops,
		MaxHops: rel.MaxHops,
		All:     part.Shortest == ast.ShortestAll,
	}
	pl.bind(part.Variable)
	pl.bind(rel.Variable)
	return nil
}

// checkShortestFilters rejects WHERE conditions on a shortest path or its
// relationships in the same MATCH. openCypher applies them during the search
// (the shortest path that satisfies them), while filtering afterwards would
// silently return fewer paths; filter in a following WITH ... WHERE instead.
func checkShortestFilters(parts []ast.PatternPart, residual []ast.Expr) error {
	vars := map[string]bool{}
	for _, p := range parts {
		if p.Shortest == ast.NotShortest {
			continue
		}
		if p.Variable != "" {
			vars[p.Variable] = true
		}
		if v := p.Chain[0].Rel.Variable; v != "" {
			vars[v] = true
		}
	}
	if len(vars) == 0 {
		return nil
	}
	for _, e := range residual {
		for _, v := range exprVars(e) {
			if vars[v] {
				return fmt.Errorf("plan: a WHERE condition on the shortest path %s is not supported in the same MATCH; filter in a following WITH ... WHERE", v)
			}
		}
	}
	return nil
}
