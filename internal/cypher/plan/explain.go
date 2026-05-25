package plan

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// Explain rende il piano come albero testuale indentato (radice in alto).
func Explain(root Op) string {
	var b strings.Builder
	explain(&b, root, 0)
	return b.String()
}

func explain(b *strings.Builder, o Op, depth int) {
	if o == nil {
		return
	}
	desc, children := describe(o)
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(desc)
	b.WriteByte('\n')
	for _, c := range children {
		explain(b, c, depth+1)
	}
}

// describe restituisce la riga di descrizione di un operatore e i suoi figli.
func describe(o Op) (string, []Op) {
	switch x := o.(type) {
	case *AllNodesScan:
		return fmt.Sprintf("AllNodesScan(%s)", x.Var), nil
	case *NodeByLabelScan:
		return fmt.Sprintf("NodeByLabelScan(%s:%s)", x.Var, x.Label), nil
	case *NodeByProperty:
		return fmt.Sprintf("NodeByProperty(%s:%s {%s = %s})", x.Var, x.Label, x.Key, exprString(x.Value)), nil
	case *Expand:
		return fmt.Sprintf("Expand(%s)%s(%s)", x.From, relArrow(x), x.To), []Op{x.Input}
	case *Filter:
		return fmt.Sprintf("Filter(%s)", exprString(x.Pred)), []Op{x.Input}
	case *Project:
		return fmt.Sprintf("Project(%s%s)", distinctPrefix(x.Distinct), projList(x.Items)), []Op{x.Input}
	case *Aggregate:
		return fmt.Sprintf("Aggregate(group=[%s] aggs=[%s])", projList(x.GroupKeys), projList(x.Aggs)), []Op{x.Input}
	case *Sort:
		return fmt.Sprintf("Sort(%s)", sortList(x.Keys)), []Op{x.Input}
	case *Skip:
		return fmt.Sprintf("Skip(%s)", exprString(x.Count)), []Op{x.Input}
	case *Limit:
		return fmt.Sprintf("Limit(%s)", exprString(x.Count)), []Op{x.Input}
	case *CartesianProduct:
		return "CartesianProduct", []Op{x.Left, x.Right}
	default:
		return "?", nil
	}
}

func relArrow(e *Expand) string {
	var inner string
	if e.Rel != "" {
		inner += e.Rel
	}
	if len(e.Types) > 0 {
		inner += ":" + strings.Join(e.Types, "|")
	}
	if e.VarLength {
		inner += "*" + hops(e.MinHops) + ".." + hops(e.MaxHops)
	}
	body := ""
	if inner != "" {
		body = "[" + inner + "]"
	}
	switch e.Dir {
	case ast.DirOut:
		return "-" + body + "->"
	case ast.DirIn:
		return "<-" + body + "-"
	default:
		return "-" + body + "-"
	}
}

func hops(n int) string {
	if n < 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

func distinctPrefix(d bool) string {
	if d {
		return "DISTINCT "
	}
	return ""
}

func projList(items []ProjItem) string {
	parts := make([]string, len(items))
	for i, it := range items {
		s := exprString(it.Expr)
		if it.Column != "" && it.Column != s {
			s += " AS " + it.Column
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}

func sortList(keys []SortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		s := exprString(k.Expr)
		if k.Desc {
			s += " DESC"
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}

// exprString rende un'espressione AST in forma testuale leggibile.
func exprString(e ast.Expr) string {
	switch ex := e.(type) {
	case nil:
		return ""
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
		var sb strings.Builder
		sb.WriteString(ex.Name)
		sb.WriteByte('(')
		if ex.Distinct {
			sb.WriteString("DISTINCT ")
		}
		if ex.Star {
			sb.WriteByte('*')
		}
		for i, a := range ex.Args {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(exprString(a))
		}
		sb.WriteByte(')')
		return sb.String()
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
