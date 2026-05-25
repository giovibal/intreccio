package sema

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
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
	default:
		return ast.Pos{}
	}
}

// exprString rende un'espressione in forma testuale, usata per nominare gli item
// di proiezione privi di alias (es. RETURN a.b -> colonna "a.b").
func exprString(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.Variable:
		return ex.Name
	case *ast.PropertyAccess:
		return exprString(ex.Target) + "." + ex.Key
	case *ast.Param:
		return "$" + ex.Name
	case *ast.Literal:
		return literalString(ex.Value)
	case *ast.Unary:
		if ex.Op == "NOT" {
			return "NOT " + exprString(ex.Expr)
		}
		return ex.Op + exprString(ex.Expr)
	case *ast.Binary:
		return exprString(ex.Left) + " " + ex.Op + " " + exprString(ex.Right)
	case *ast.LabelsPredicate:
		return exprString(ex.Expr) + ":" + strings.Join(ex.Labels, ":")
	case *ast.FunctionCall:
		var b strings.Builder
		b.WriteString(ex.Name)
		b.WriteByte('(')
		if ex.Distinct {
			b.WriteString("DISTINCT ")
		}
		if ex.Star {
			b.WriteByte('*')
		}
		for i, arg := range ex.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(exprString(arg))
		}
		b.WriteByte(')')
		return b.String()
	default:
		return "?"
	}
}

func literalString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "'" + x + "'"
	default:
		return fmt.Sprintf("%v", x)
	}
}
