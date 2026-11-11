package intreccio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/exec"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/plan"
)

// anchoringEnabled lets tests and benchmarks force every live query down the
// full re-evaluation path.
var anchoringEnabled = true

// anchor re-evaluates a live query around the nodes that changed instead of
// running it again in full (ADR 0010, "v2").
//
// It applies to queries of the shape `MATCH pattern [WHERE …] RETURN items`
// with fixed-length relationships and a plain projection. For such a query the
// result is one row per match of the pattern, and a match can only appear,
// disappear or change when one of the nodes it binds changes — a relationship
// change counts as a change to both of its ends. So, given the set of changed
// nodes:
//
//   - the rows that may be stale are exactly those of the matches that bind one
//     of them, found through an index kept from the previous evaluation;
//   - the rows that replace them are found by matching the pattern outward
//     from each changed node, once per position it could occupy.
//
// The cost follows the neighbourhood of the change, not the size of the result.
// The price is the index: the anchor keeps every match's row and node IDs.
type anchor struct {
	// query is the live query rewritten so that every pattern element has a
	// name and the projection ends with the ID of each of them.
	query   *ast.Query
	columns []string // the query's own columns, then the hidden ID columns
	nCols   int      // how many of columns are the query's own
	// nodeVars are the pattern's distinct node variables: the positions a
	// changed node is tried at. The first len(nodeVars) hidden columns are
	// their IDs; the remaining ones are relationship IDs.
	nodeVars []string

	built   bool
	matches map[string]*match              // by match key
	byNode  map[uint64]map[string]struct{} // node ID -> keys of the matches binding it
	rows    int64                          // rows in the result: the sum of the matches' counts
}

// match is one match of the pattern: the row it produces and the nodes it binds.
//
// count is how many times the executor produced it. That is normally once, but
// the index must hold whatever the query returns, not what it should return:
// the same binding emitted twice is two rows of the result.
type match struct {
	row   []byte // canonical encoding of the projected row
	nodes []uint64
	count int
}

// hiddenPrefix starts the names the rewrite introduces. No identifier a query
// can contain starts with a NUL byte, so they cannot clash with the query's.
const hiddenPrefix = "\x00"

var aggregateFuncs = map[string]bool{"count": true, "collect": true, "sum": true, "avg": true, "min": true, "max": true}

// newAnchor returns an anchor for q, or nil when q is not of a shape that can
// be re-evaluated around a node; such a query is re-run in full instead.
func newAnchor(q *ast.Query, columns []string) *anchor {
	if len(q.Unions) > 0 || len(q.Clauses) != 2 {
		return nil
	}
	m, ok := q.Clauses[0].(*ast.Match)
	if !ok || m.Optional {
		return nil
	}
	r, ok := q.Clauses[1].(*ast.Return)
	if !ok || r.Distinct || len(r.OrderBy) > 0 || r.Skip != nil || r.Limit != nil {
		return nil
	}
	for _, it := range r.Items {
		if hasAggregate(it.Expr) {
			return nil
		}
	}

	a := &anchor{nCols: len(columns)}
	var relVars []string
	seen := map[string]bool{}
	synth := 0
	name := func(given string) string {
		if given != "" {
			return given
		}
		synth++
		return fmt.Sprintf("%s%d", hiddenPrefix, synth)
	}
	parts := make([]ast.PatternPart, len(m.Parts))
	for i, part := range m.Parts {
		if part.Variable != "" || part.Start == nil {
			return nil // path variables are not supported
		}
		node := func(n *ast.NodePattern) *ast.NodePattern {
			cp := *n
			cp.Variable = name(n.Variable)
			if !seen[cp.Variable] {
				seen[cp.Variable] = true
				a.nodeVars = append(a.nodeVars, cp.Variable)
			}
			return &cp
		}
		out := ast.PatternPart{Start: node(part.Start)}
		for _, step := range part.Chain {
			if step.Rel == nil || step.Node == nil || step.Rel.VarLength {
				// A variable-length match depends on nodes it does not bind.
				return nil
			}
			rel := *step.Rel
			rel.Variable = name(step.Rel.Variable)
			if !seen[rel.Variable] {
				seen[rel.Variable] = true
				relVars = append(relVars, rel.Variable)
			}
			out.Chain = append(out.Chain, ast.PatternChain{Rel: &rel, Node: node(step.Node)})
		}
		parts[i] = out
	}

	items := append([]ast.ReturnItem(nil), r.Items...)
	a.columns = append([]string(nil), columns...)
	for i, v := range append(append([]string(nil), a.nodeVars...), relVars...) {
		alias := fmt.Sprintf("%sid%d", hiddenPrefix, i)
		items = append(items, ast.ReturnItem{
			Expr:  &ast.FunctionCall{Name: "id", Args: []ast.Expr{&ast.Variable{Name: v}}},
			Alias: alias,
		})
		a.columns = append(a.columns, alias)
	}
	a.query = &ast.Query{Clauses: []ast.Clause{
		&ast.Match{Parts: parts, Where: m.Where, Pos: m.Pos},
		&ast.Return{Star: r.Star, Items: items, Pos: r.Pos},
	}}
	return a
}

// hasAggregate reports whether e contains an aggregate function call.
func hasAggregate(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.FunctionCall:
		if aggregateFuncs[strings.ToLower(x.Name)] {
			return true
		}
		for _, arg := range x.Args {
			if hasAggregate(arg) {
				return true
			}
		}
	case *ast.PropertyAccess:
		return hasAggregate(x.Target)
	case *ast.Unary:
		return hasAggregate(x.Expr)
	case *ast.Binary:
		return hasAggregate(x.Left) || hasAggregate(x.Right)
	case *ast.LabelsPredicate:
		return hasAggregate(x.Expr)
	case *ast.ListLiteral:
		for _, el := range x.Elements {
			if hasAggregate(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, el := range x.Entries {
			if hasAggregate(el) {
				return true
			}
		}
	case *ast.Case:
		if x.Operand != nil && hasAggregate(x.Operand) {
			return true
		}
		for _, w := range x.Whens {
			if hasAggregate(w.Cond) || hasAggregate(w.Result) {
				return true
			}
		}
		return x.Else != nil && hasAggregate(x.Else)
	}
	return false
}

// found is a match produced by an evaluation, with its decoded row.
type found struct {
	key string
	match
	decoded []any
}

// split turns the rows of the rewritten query into matches: it exports and
// encodes the query's own columns and reads the hidden ID columns. Rows with
// the same key are one match with a count, at the position of the first.
func (a *anchor) split(txn storage.Txn, rows [][]any) ([]found, error) {
	own := make([][]any, len(rows))
	for i, row := range rows {
		own[i] = row[:a.nCols:a.nCols]
	}
	if err := newExporter(txn).rows(own); err != nil {
		return nil, err
	}
	out := make([]found, 0, len(rows))
	at := make(map[string]int, len(rows))
	for i, row := range rows {
		ids := row[a.nCols:]
		key := make([]byte, 0, len(ids)*binary.MaxVarintLen64)
		nodes := make([]uint64, 0, len(a.nodeVars))
		for j, v := range ids {
			id, ok := v.(int64)
			if !ok {
				return nil, fmt.Errorf("intreccio: live query: pattern element without an ID (%T)", v)
			}
			key = binary.AppendUvarint(key, uint64(id))
			if j < len(a.nodeVars) {
				nodes = append(nodes, uint64(id))
			}
		}
		if j, dup := at[string(key)]; dup {
			out[j].count++
			continue
		}
		enc, err := encodeRow(own[i])
		if err != nil {
			return nil, err
		}
		at[string(key)] = len(out)
		out = append(out, found{key: string(key), match: match{row: enc, nodes: nodes, count: 1}, decoded: own[i]})
	}
	return out, nil
}

func (a *anchor) insert(f found) {
	m := f.match
	a.matches[f.key] = &m
	a.rows += int64(m.count)
	for _, id := range m.nodes {
		set := a.byNode[id]
		if set == nil {
			set = map[string]struct{}{}
			a.byNode[id] = set
		}
		set[f.key] = struct{}{}
	}
}

func (a *anchor) remove(key string) {
	m := a.matches[key]
	if m == nil {
		return
	}
	delete(a.matches, key)
	a.rows -= int64(m.count)
	for _, id := range m.nodes {
		set := a.byNode[id]
		delete(set, key)
		if len(set) == 0 {
			delete(a.byNode, id)
		}
	}
}

// rebuild runs the query in full and replaces the index, reporting how the
// result differs from what the index held. It is the first evaluation, and the
// fallback whenever the changes cannot be listed.
func (a *anchor) rebuild(txn storage.Txn, lq *liveQuery) (*evaluation, error) {
	p, err := plan.Plan(a.query, planCatalog{txn: txn})
	if err != nil {
		return nil, err
	}
	rows, err := exec.Run(p, a.columns, &exec.Context{Txn: txn, Params: lq.params, Ctx: lq.ctx, MaxRows: lq.maxRows})
	if err != nil {
		return nil, err
	}
	matches, err := a.split(txn, rows)
	if err != nil {
		return nil, err
	}

	var before changefeed.ResultSet
	old := make([][]byte, 0, a.rows)
	for _, m := range a.matches {
		for range m.count {
			old = append(old, m.row)
		}
	}
	sort.Slice(old, func(i, j int) bool { return bytes.Compare(old[i], old[j]) < 0 })
	before.Replace(old)

	a.matches = make(map[string]*match, len(matches))
	a.byNode = map[uint64]map[string]struct{}{}
	a.rows = 0
	a.built = true
	var (
		cur  [][]byte
		from []int // for each row of cur, the match it comes from
	)
	for i, f := range matches {
		a.insert(f)
		for range f.count {
			cur = append(cur, f.row)
			from = append(from, i)
		}
	}
	addedIdx, removed := before.Replace(cur)
	ev := &evaluation{removed: removed}
	for _, i := range addedIdx {
		ev.added = append(ev.added, matches[from[i]].decoded)
		ev.addedEnc = append(ev.addedEnc, cur[i])
	}
	return ev, nil
}

// refresh re-evaluates the query around the given nodes and updates the index,
// reporting how the result changed.
func (a *anchor) refresh(txn storage.Txn, lq *liveQuery, dirty []uint64) (*evaluation, error) {
	// The matches that may be stale: every one that binds a changed node.
	stale := map[string]struct{}{}
	for _, id := range dirty {
		for key := range a.byNode[id] {
			stale[key] = struct{}{}
		}
	}

	// The matches that hold now: the pattern matched outward from each changed
	// node, at each position it could occupy.
	plans := make(map[string]plan.Op, len(a.nodeVars))
	fresh := map[string]int{} // match key -> index into order
	var order []found
	for _, id := range dirty {
		node, err := graph.GetNode(txn, id)
		if errors.Is(err, graph.ErrNodeNotFound) {
			continue // deleted: it only had stale matches
		}
		if err != nil {
			return nil, err
		}
		for _, v := range a.nodeVars {
			p := plans[v]
			if p == nil {
				if p, err = plan.PlanSeeded(a.query, planCatalog{txn: txn}, []string{v}); err != nil {
					return nil, err
				}
				plans[v] = p
			}
			rows, err := exec.RunSeeded(p, a.columns, &exec.Context{Txn: txn, Params: lq.params, Ctx: lq.ctx}, map[string]any{v: node})
			if err != nil {
				return nil, err
			}
			matches, err := a.split(txn, rows)
			if err != nil {
				return nil, err
			}
			for _, f := range matches {
				if _, dup := fresh[f.key]; dup {
					// The same match, reached from another node or position. Its
					// count is that of the first run to find it: every run
					// produces a given binding the same number of times.
					continue
				}
				fresh[f.key] = len(order)
				order = append(order, f)
			}
		}
	}

	// Rows leaving: stale matches that are gone or that changed.
	staleKeys := make([]string, 0, len(stale))
	for key := range stale {
		staleKeys = append(staleKeys, key)
	}
	sort.Strings(staleKeys)
	same := func(old *match, f found) bool { return old.count == f.count && bytes.Equal(old.row, f.row) }
	var removed [][]byte
	for _, key := range staleKeys {
		old := a.matches[key]
		if i, ok := fresh[key]; ok && same(old, order[i]) {
			continue
		}
		for range old.count {
			removed = append(removed, old.row)
		}
	}
	// Rows entering: fresh matches that are new or that changed.
	var added []found
	for _, f := range order {
		if old := a.matches[f.key]; old != nil && same(old, f) {
			continue
		}
		for range f.count {
			added = append(added, f)
		}
	}

	for _, key := range staleKeys {
		a.remove(key)
	}
	for _, f := range order {
		a.insert(f)
	}
	if lq.maxRows > 0 && a.rows > lq.maxRows {
		return nil, exec.ErrResultLimit
	}

	// The result is a multiset of rows: a row that left through one match and
	// came back through another did not change it.
	leaving := map[string]int{}
	for _, row := range removed {
		leaving[string(row)]++
	}
	ev := &evaluation{}
	for _, f := range added {
		if leaving[string(f.row)] > 0 {
			leaving[string(f.row)]--
			continue
		}
		ev.added = append(ev.added, f.decoded)
		ev.addedEnc = append(ev.addedEnc, f.row)
	}
	for _, row := range removed {
		if leaving[string(row)] > 0 {
			leaving[string(row)]--
			ev.removed = append(ev.removed, row)
		}
	}
	return ev, nil
}
