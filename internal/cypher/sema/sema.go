// Package sema esegue l'analisi semantica dell'AST Cypher: risoluzione dello
// scope delle variabili (con reset su WITH), validazioni di base e calcolo delle
// colonne di output. Il binding di label/tipi/proprietà agli ID di dizionario è
// deferito a plan/exec, dove è disponibile una transazione (vedi ADR 0004).
package sema

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// SemaError è un errore semantico con posizione nel sorgente.
type SemaError struct {
	Pos ast.Pos
	Msg string
}

func (e *SemaError) Error() string {
	return fmt.Sprintf("riga %d:%d: %s", e.Pos.Line, e.Pos.Col, e.Msg)
}

// Result è l'esito dell'analisi: le colonne prodotte dalla query (vuoto per
// query di sola scrittura senza RETURN).
type Result struct {
	Columns []string
}

// Analyze valida la query e ne calcola le colonne di output.
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
		return nil, &SemaError{Msg: "query vuota"}
	}
	if hasCreateIndex(q) && len(q.Clauses) != 1 {
		return nil, a.errf(clausePos(q.Clauses[0]), "CREATE INDEX dev'essere l'unica clausola")
	}

	sc := newScope()
	for i, clause := range q.Clauses {
		last := i == len(q.Clauses)-1
		if _, ok := clause.(*ast.Return); ok && !last {
			return nil, a.errf(clausePos(clause), "RETURN dev'essere l'ultima clausola")
		}
		if err := a.clause(sc, clause); err != nil {
			return nil, err
		}
	}

	if !endsQuery(q.Clauses[len(q.Clauses)-1]) {
		return nil, a.errf(clausePos(q.Clauses[len(q.Clauses)-1]),
			"query incompleta: serve RETURN o una clausola di scrittura")
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
		return a.introducePattern(sc, []ast.PatternPart{cl.Part})
	case *ast.Set:
		return a.set(sc, cl)
	case *ast.Delete:
		return a.delete(sc, cl)
	case *ast.With:
		return a.with(sc, cl)
	case *ast.Return:
		return a.ret(sc, cl)
	case *ast.CreateIndex:
		return nil // nessuna variabile in gioco
	default:
		return a.errf(clausePos(c), "clausola non supportata")
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
		if err := a.checkExpr(sc, item.Target, false); err != nil {
			return err
		}
		if err := a.checkExpr(sc, item.Value, false); err != nil {
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

// introducePattern aggiunge allo scope le variabili del pattern (nodi, relazioni,
// path) e valida le espressioni nelle mappe di proprietà contro lo scope risultante.
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
	// Seconda passata: le mappe di proprietà possono riferire variabili del pattern.
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
	// Gli item sono valutati nello scope corrente (pre-WITH).
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
				return a.errf(exprPos(item.Expr), "le espressioni in WITH devono avere un alias (AS)")
			}
			name = v.Name
		}
		next.define(name, exprPos(item.Expr))
	}

	// ORDER BY/SKIP/LIMIT/WHERE riferiscono le colonne proiettate.
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
		return a.errf(r.Pos, "RETURN * non è ammesso senza variabili in scope")
	}

	// ORDER BY in RETURN può riferire sia le variabili in scope sia gli alias proiettati.
	tail := newScope()
	tail.merge(sc)
	cols := a.columns(sc, r.Star, r.Items, tail)
	if err := a.checkProjectionTail(tail, r.OrderBy, r.Skip, r.Limit); err != nil {
		return err
	}
	a.result.Columns = cols
	return nil
}

// columns calcola i nomi delle colonne di output e li registra anche in tail
// (così ORDER BY può riferirle).
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

// checkExpr valida i riferimenti a variabile e l'uso delle aggregazioni.
func (a *analyzer) checkExpr(sc *scope, e ast.Expr, inAgg bool) error {
	switch ex := e.(type) {
	case *ast.Literal, *ast.Param:
		return nil
	case *ast.Variable:
		if !sc.has(ex.Name) {
			return a.errf(ex.Pos, "variabile non definita: %s", ex.Name)
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
	case *ast.FunctionCall:
		agg := isAggregate(ex.Name)
		if agg && inAgg {
			return a.errf(ex.Pos, "aggregazione annidata in un'altra aggregazione")
		}
		childAgg := inAgg || agg
		for _, arg := range ex.Args {
			if err := a.checkExpr(sc, arg, childAgg); err != nil {
				return err
			}
		}
		return nil
	default:
		return a.errf(exprPos(e), "espressione non supportata")
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

// endsQuery indica se la clausola può chiudere una query.
func endsQuery(c ast.Clause) bool {
	switch c.(type) {
	case *ast.Return, *ast.Create, *ast.Merge, *ast.Set, *ast.Delete, *ast.CreateIndex:
		return true
	default:
		return false
	}
}
