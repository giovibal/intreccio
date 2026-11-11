package changefeed

import (
	"strings"

	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/sema"
)

// FootprintOf extracts the footprint of a read query from its AST: which
// labels, relationship types and property keys its result can depend on.
//
// The rules are deliberately simple and err on the side of waking:
//   - a node pattern contributes its labels, or AnyLabel when it has none;
//   - a relationship pattern contributes its types, or AnyType when it has none;
//   - `v.key` contributes key; a key in a node pattern's property map does too;
//   - any other use of a pattern variable (returning it, passing it to a
//     function, renaming it in WITH, RETURN *) may expose all of its
//     properties and sets AnyKey — except as the argument of id(), labels(),
//     type() or count(), or as the subject of a label predicate, none of which
//     read a property.
//
// Relationship properties never narrow anything: every change to a
// relationship of a type in Types — created, deleted or a property set —
// concerns the query.
func FootprintOf(q *ast.Query) Footprint {
	b := &footprintBuilder{
		fp: Footprint{
			Labels: map[string]struct{}{},
			Types:  map[string]struct{}{},
			Keys:   map[string]struct{}{},
		},
		patternVars: map[string]struct{}{},
	}
	arms := [][]ast.Clause{q.Clauses}
	for _, u := range q.Unions {
		arms = append(arms, u.Clauses)
	}
	// Pattern variables first: an expression is judged by whether the names it
	// uses are graph entities anywhere in the query.
	for _, arm := range arms {
		for _, c := range arm {
			if sema.IsWriteClause(c) {
				return everything()
			}
			if m, ok := c.(*ast.Match); ok {
				b.patterns(m.Parts)
			}
		}
	}
	for _, arm := range arms {
		for _, c := range arm {
			if !b.clause(c) {
				return everything()
			}
		}
	}
	return b.fp
}

// everything is the footprint of a query that cannot be analysed: any change
// concerns it.
func everything() Footprint {
	return Footprint{AnyLabel: true, AnyType: true, AnyKey: true}
}

type footprintBuilder struct {
	fp          Footprint
	patternVars map[string]struct{}
}

func (b *footprintBuilder) patterns(parts []ast.PatternPart) {
	for _, part := range parts {
		if part.Variable != "" {
			b.patternVars[part.Variable] = struct{}{}
		}
		if part.Variable != "" || part.Shortest != ast.NotShortest {
			// A path carries nodes the pattern does not constrain: any node on
			// it may show up in the result, whatever its labels.
			b.fp.AnyLabel = true
		}
		b.nodePattern(part.Start)
		for _, step := range part.Chain {
			b.relPattern(step.Rel)
			b.nodePattern(step.Node)
		}
	}
}

func (b *footprintBuilder) nodePattern(n *ast.NodePattern) {
	if n == nil {
		return
	}
	if n.Variable != "" {
		b.patternVars[n.Variable] = struct{}{}
	}
	if len(n.Labels) == 0 {
		b.fp.AnyLabel = true
	}
	for _, l := range n.Labels {
		b.fp.Labels[l] = struct{}{}
	}
	for k := range n.Props {
		b.fp.Keys[k] = struct{}{}
	}
}

func (b *footprintBuilder) relPattern(r *ast.RelPattern) {
	if r == nil {
		return
	}
	if r.Variable != "" {
		b.patternVars[r.Variable] = struct{}{}
	}
	if len(r.Types) == 0 {
		b.fp.AnyType = true
	}
	for _, t := range r.Types {
		b.fp.Types[t] = struct{}{}
	}
}

// clause walks the expressions of a read clause. It returns false for a clause
// it does not know, so that new clause kinds fail safe.
func (b *footprintBuilder) clause(c ast.Clause) bool {
	switch cl := c.(type) {
	case *ast.Match:
		for _, part := range cl.Parts {
			b.patternExprs(part)
		}
		b.expr(cl.Where)
	case *ast.With:
		b.projection(cl.Star, cl.Items, cl.OrderBy, cl.Skip, cl.Limit)
		b.expr(cl.Where)
	case *ast.Return:
		b.projection(cl.Star, cl.Items, cl.OrderBy, cl.Skip, cl.Limit)
	case *ast.Unwind:
		b.expr(cl.Expr)
	default:
		return false
	}
	return true
}

// patternExprs walks the value expressions of the property maps in a pattern.
func (b *footprintBuilder) patternExprs(part ast.PatternPart) {
	if part.Start != nil {
		for _, e := range part.Start.Props {
			b.expr(e)
		}
	}
	for _, step := range part.Chain {
		if step.Rel != nil {
			for _, e := range step.Rel.Props {
				b.expr(e)
			}
		}
		if step.Node != nil {
			for _, e := range step.Node.Props {
				b.expr(e)
			}
		}
	}
}

func (b *footprintBuilder) projection(star bool, items []ast.ReturnItem, orderBy []ast.SortItem, skip, limit ast.Expr) {
	if star {
		b.fp.AnyKey = true // projects every variable in scope, entities included
	}
	for _, it := range items {
		b.expr(it.Expr)
	}
	for _, s := range orderBy {
		b.expr(s.Expr)
	}
	b.expr(skip)
	b.expr(limit)
}

// propertyFree lists the functions that read no property of an entity passed to
// them directly.
var propertyFree = map[string]bool{"id": true, "labels": true, "type": true, "count": true}

func (b *footprintBuilder) expr(e ast.Expr) {
	switch x := e.(type) {
	case nil:
	case *ast.Literal, *ast.Param:
	case *ast.Variable:
		if _, ok := b.patternVars[x.Name]; ok {
			b.fp.AnyKey = true
		}
	case *ast.PropertyAccess:
		b.fp.Keys[x.Key] = struct{}{}
		if _, direct := x.Target.(*ast.Variable); !direct {
			b.expr(x.Target)
		}
	case *ast.Unary:
		b.expr(x.Expr)
	case *ast.Binary:
		b.expr(x.Left)
		b.expr(x.Right)
	case *ast.FunctionCall:
		free := propertyFree[strings.ToLower(x.Name)]
		for _, arg := range x.Args {
			if _, direct := arg.(*ast.Variable); direct && free {
				continue
			}
			b.expr(arg)
		}
	case *ast.LabelsPredicate:
		if _, direct := x.Expr.(*ast.Variable); !direct {
			b.expr(x.Expr)
		}
	case *ast.ListLiteral:
		for _, el := range x.Elements {
			b.expr(el)
		}
	case *ast.MapLiteral:
		for _, el := range x.Entries {
			b.expr(el)
		}
	case *ast.Case:
		b.expr(x.Operand)
		for _, w := range x.Whens {
			b.expr(w.Cond)
			b.expr(w.Result)
		}
		b.expr(x.Else)
	default:
		b.fp.AnyKey = true // an expression kind this walker does not know
	}
}
