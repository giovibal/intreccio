package sema

import (
	"github.com/giovibal/intreccio/query/ast"
)

func clausePos(c ast.Clause) ast.Pos {
	switch cl := c.(type) {
	case *ast.Match:
		return cl.Pos
	case *ast.With:
		return cl.Pos
	case *ast.Return:
		return cl.Pos
	case *ast.Create:
		return cl.Pos
	case *ast.Merge:
		return cl.Pos
	case *ast.Set:
		return cl.Pos
	case *ast.Delete:
		return cl.Pos
	case *ast.CreateIndex:
		return cl.Pos
	case *ast.Remove:
		return cl.Pos
	case *ast.Unwind:
		return cl.Pos
	default:
		return ast.Pos{}
	}
}

func exprPos(e ast.Expr) ast.Pos {
	switch ex := e.(type) {
	case *ast.Literal:
		return ex.Pos
	case *ast.Param:
		return ex.Pos
	case *ast.Variable:
		return ex.Pos
	case *ast.PropertyAccess:
		return ex.Pos
	case *ast.Unary:
		return ex.Pos
	case *ast.Binary:
		return ex.Pos
	case *ast.FunctionCall:
		return ex.Pos
	case *ast.LabelsPredicate:
		return ex.Pos
	case *ast.Subscript:
		return ex.Pos
	case *ast.Slice:
		return ex.Pos
	case *ast.ListComprehension:
		return ex.Pos
	case *ast.Quantifier:
		return ex.Pos
	default:
		return ast.Pos{}
	}
}

// exprString renders an expression as text, used to name projection items that
// lack an alias (e.g. RETURN a.b -> column "a.b"). It is shared with the
// planner through ast.Format so both agree on column names.
func exprString(e ast.Expr) string { return ast.Format(e) }
