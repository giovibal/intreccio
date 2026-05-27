// Package sema performs semantic analysis of the Cypher AST: variable scope
// resolution (with reset on WITH), basic validations and computation of the
// output columns. Binding of labels/types/properties to dictionary IDs is
// deferred to plan/exec, where a transaction is available (see ADR 0004).
package sema

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// SemaError is a semantic error with a position in the source.
type SemaError struct {
	Pos ast.Pos
	Msg string
}

func (e *SemaError) Error() string {
	return fmt.Sprintf("line %d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg)
}

// Result is the outcome of the analysis: the columns produced by the query
// (empty for write-only queries without RETURN).
type Result struct {
	Columns []string
}

// Analyze validates the query and computes its output columns.
func Analyze(q *ast.Query) (*Result, error) {
	a := &analyzer{}
	return a.run(q)
}

type analyzer struct {
	result Result
}

func (a *analyzer) errf(pos ast.Pos, format string, args ...any) *SemaError {
	return &SemaError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (a *analyzer) run(q *ast.Query) (*Result, error) {
	if len(q.Clauses) == 0 {
		return nil, &SemaError{Msg: "empty query"}
	}
	if hasCreateIndex(q) && len(q.Clauses) != 1 {
		return nil, a.errf(clausePos(q.Clauses[0]), "CREATE INDEX must be the only clause")
	}

	sc := newScope()
	for i, clause := range q.Clauses {
		last := i == len(q.Clauses)-1
		if _, ok := clause.(*ast.Return); ok && !last {
			return nil, a.errf(clausePos(clause), "RETURN must be the last clause")
		}
		if err := a.clause(sc, clause); err != nil {
			return nil, err
		}
	}

	if !endsQuery(q.Clauses[len(q.Clauses)-1]) {
		return nil, a.errf(clausePos(q.Clauses[len(q.Clauses)-1]),
			"incomplete query: a RETURN or a writing clause is required")
	}
	return &a.result, nil
}

func (a *analyzer) clause(sc *scope, c ast.Clause) error {
	switch cl := c.(type) {
	case *ast.Match:
		return a.match(sc, cl)
	case *ast.Create:
		return a.create(sc, cl)
	case *ast.Merge:
		return a.merge(sc, cl)
	case *ast.Set:
		return a.set(sc, cl)
	case *ast.Remove:
		return a.remove(sc, cl)
	case *ast.Unwind:
		return a.unwind(sc, cl)
	case *ast.Delete:
		return a.delete(sc, cl)
	case *ast.With:
		return a.with(sc, cl)
	case *ast.Return:
		return a.ret(sc, cl)
	case *ast.CreateIndex:
		return nil // no variables involved
	default:
		return a.errf(clausePos(c), "unsupported clause")
	}
}

func (a *analyzer) match(sc *scope, m *ast.Match) error {
	if err := a.introducePattern(sc, m.Parts); err != nil {
		return err
	}
	if m.Where != nil {
		return a.checkExpr(sc, m.Where, false)
	}
	return nil
}

func (a *analyzer) create(sc *scope, c *ast.Create) error {
	return a.introducePattern(sc, c.Parts)
}

func (a *analyzer) set(sc *scope, s *ast.Set) error {
	for _, item := range s.Items {
		if err := a.checkSetClause(sc, item); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) checkSetClause(sc *scope, item ast.SetClause) error {
	switch it := item.(type) {
	case *ast.SetProperty:
		if err := a.checkExpr(sc, it.Target, false); err != nil {
			return err
		}
		return a.checkExpr(sc, it.Value, false)
	case *ast.SetLabels:
		if !sc.has(it.Variable) {
			return a.errf(it.Pos, "undefined variable: %s", it.Variable)
		}
		return nil
	case *ast.SetMap:
		if !sc.has(it.Variable) {
			return a.errf(it.Pos, "undefined variable: %s", it.Variable)
		}
		return a.checkExpr(sc, it.Value, false)
	default:
		return a.errf(ast.Pos{}, "unsupported SET clause %T", item)
	}
}

func (a *analyzer) remove(sc *scope, r *ast.Remove) error {
	for _, item := range r.Items {
		switch it := item.(type) {
		case *ast.RemoveProperty:
			if err := a.checkExpr(sc, it.Target, false); err != nil {
				return err
			}
		case *ast.RemoveLabels:
			if !sc.has(it.Variable) {
				return a.errf(it.Pos, "undefined variable: %s", it.Variable)
			}
		default:
			return a.errf(ast.Pos{}, "unsupported REMOVE clause %T", item)
		}
	}
	return nil
}

func (a *analyzer) unwind(sc *scope, u *ast.Unwind) error {
	if err := a.checkExpr(sc, u.Expr, false); err != nil {
		return err
	}
	sc.define(u.Alias, u.Pos)
	return nil
}

func (a *analyzer) merge(sc *scope, m *ast.Merge) error {
	if err := a.introducePattern(sc, []ast.PatternPart{m.Part}); err != nil {
		return err
	}
	for _, item := range m.OnCreate {
		if err := a.checkSetClause(sc, item); err != nil {
			return err
		}
	}
	for _, item := range m.OnMatch {
		if err := a.checkSetClause(sc, item); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) delete(sc *scope, d *ast.Delete) error {
	for _, e := range d.Exprs {
		if err := a.checkExpr(sc, e, false); err != nil {
			return err
		}
	}
	return nil
}

// introducePattern adds the pattern variables (nodes, relationships, path) to the
// scope and validates the expressions in the property maps against the resulting
// scope.
func (a *analyzer) introducePattern(sc *scope, parts []ast.PatternPart) error {
	for _, part := range parts {
		if part.Variable != "" {
			sc.define(part.Variable, part.Start.Pos)
		}
		a.defineNode(sc, part.Start)
		for _, ch := range part.Chain {
			if ch.Rel.Variable != "" {
				sc.define(ch.Rel.Variable, ch.Rel.Pos)
			}
			a.defineNode(sc, ch.Node)
		}
	}
	// Second pass: property maps may reference pattern variables.
	for _, part := range parts {
		if err := a.checkProps(sc, part.Start.Props); err != nil {
			return err
		}
		for _, ch := range part.Chain {
			if err := a.checkProps(sc, ch.Rel.Props); err != nil {
				return err
			}
			if err := a.checkProps(sc, ch.Node.Props); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *analyzer) defineNode(sc *scope, n *ast.NodePattern) {
	if n.Variable != "" {
		sc.define(n.Variable, n.Pos)
	}
}

func (a *analyzer) checkProps(sc *scope, props map[string]ast.Expr) error {
	for _, v := range props {
		if err := a.checkExpr(sc, v, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) with(sc *scope, w *ast.With) error {
	// Items are evaluated in the current (pre-WITH) scope.
	for _, item := range w.Items {
		if err := a.checkExpr(sc, item.Expr, false); err != nil {
			return err
		}
	}

	next := newScope()
	if w.Star {
		next.merge(sc)
	}
	for _, item := range w.Items {
		name := item.Alias
		if name == "" {
			v, ok := item.Expr.(*ast.Variable)
			if !ok {
				return a.errf(exprPos(item.Expr), "expressions in WITH must have an alias (AS)")
			}
			name = v.Name
		}
		next.define(name, exprPos(item.Expr))
	}

	// ORDER BY/SKIP/LIMIT/WHERE reference the projected columns.
	if err := a.checkProjectionTail(next, w.OrderBy, w.Skip, w.Limit); err != nil {
		return err
	}
	if w.Where != nil {
		if err := a.checkExpr(next, w.Where, false); err != nil {
			return err
		}
	}

	sc.replaceWith(next)
	return nil
}

func (a *analyzer) ret(sc *scope, r *ast.Return) error {
	for _, item := range r.Items {
		if err := a.checkExpr(sc, item.Expr, false); err != nil {
			return err
		}
	}
	if r.Star && sc.empty() && len(r.Items) == 0 {
		return a.errf(r.Pos, "RETURN * is not allowed with no variables in scope")
	}

	// ORDER BY in RETURN may reference both the in-scope variables and the projected aliases.
	tail := newScope()
	tail.merge(sc)
	cols := a.columns(sc, r.Star, r.Items, tail)
	if err := a.checkProjectionTail(tail, r.OrderBy, r.Skip, r.Limit); err != nil {
		return err
	}
	a.result.Columns = cols
	return nil
}

// columns computes the output column names and also registers them in tail (so
// ORDER BY can reference them).
func (a *analyzer) columns(sc *scope, star bool, items []ast.ReturnItem, tail *scope) []string {
	var cols []string
	if star {
		cols = append(cols, sc.order...)
	}
	for _, item := range items {
		name := item.Alias
		if name == "" {
			name = exprString(item.Expr)
		}
		cols = append(cols, name)
		tail.define(name, exprPos(item.Expr))
	}
	return cols
}

func (a *analyzer) checkProjectionTail(sc *scope, orderBy []ast.SortItem, skip, limit ast.Expr) error {
	for _, s := range orderBy {
		if err := a.checkExpr(sc, s.Expr, false); err != nil {
			return err
		}
	}
	if skip != nil {
		if err := a.checkExpr(sc, skip, false); err != nil {
			return err
		}
	}
	if limit != nil {
		if err := a.checkExpr(sc, limit, false); err != nil {
			return err
		}
	}
	return nil
}

// checkExpr validates variable references and the use of aggregations.
func (a *analyzer) checkExpr(sc *scope, e ast.Expr, inAgg bool) error {
	switch ex := e.(type) {
	case *ast.Literal, *ast.Param:
		return nil
	case *ast.Variable:
		if !sc.has(ex.Name) {
			return a.errf(ex.Pos, "undefined variable: %s", ex.Name)
		}
		return nil
	case *ast.PropertyAccess:
		return a.checkExpr(sc, ex.Target, inAgg)
	case *ast.Unary:
		return a.checkExpr(sc, ex.Expr, inAgg)
	case *ast.Binary:
		if err := a.checkExpr(sc, ex.Left, inAgg); err != nil {
			return err
		}
		return a.checkExpr(sc, ex.Right, inAgg)
	case *ast.LabelsPredicate:
		return a.checkExpr(sc, ex.Expr, inAgg)
	case *ast.ListLiteral:
		for _, el := range ex.Elements {
			if err := a.checkExpr(sc, el, inAgg); err != nil {
				return err
			}
		}
		return nil
	case *ast.MapLiteral:
		for _, v := range ex.Entries {
			if err := a.checkExpr(sc, v, inAgg); err != nil {
				return err
			}
		}
		return nil
	case *ast.Case:
		if ex.Operand != nil {
			if err := a.checkExpr(sc, ex.Operand, inAgg); err != nil {
				return err
			}
		}
		for _, w := range ex.Whens {
			if err := a.checkExpr(sc, w.Cond, inAgg); err != nil {
				return err
			}
			if err := a.checkExpr(sc, w.Result, inAgg); err != nil {
				return err
			}
		}
		if ex.Else != nil {
			if err := a.checkExpr(sc, ex.Else, inAgg); err != nil {
				return err
			}
		}
		return nil
	case *ast.FunctionCall:
		agg := isAggregate(ex.Name)
		if agg && inAgg {
			return a.errf(ex.Pos, "aggregation nested inside another aggregation")
		}
		childAgg := inAgg || agg
		for _, arg := range ex.Args {
			if err := a.checkExpr(sc, arg, childAgg); err != nil {
				return err
			}
		}
		return nil
	default:
		return a.errf(exprPos(e), "unsupported expression")
	}
}

var aggregates = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true, "collect": true,
}

func isAggregate(name string) bool { return aggregates[strings.ToLower(name)] }

func hasCreateIndex(q *ast.Query) bool {
	for _, c := range q.Clauses {
		if _, ok := c.(*ast.CreateIndex); ok {
			return true
		}
	}
	return false
}

// endsQuery reports whether the clause can terminate a query.
func endsQuery(c ast.Clause) bool {
	switch c.(type) {
	case *ast.Return, *ast.Create, *ast.Merge, *ast.Set, *ast.Remove, *ast.Delete, *ast.CreateIndex:
		return true
	default:
		return false
	}
}
