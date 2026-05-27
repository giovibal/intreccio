package plan

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/cypher/ast"
)

// Explain renders the plan as an indented text tree (root at the top).
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

// describe returns the description line of an operator and its children.
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
	case *Create:
		return fmt.Sprintf("Create(%s)", patternsString(x.Parts)), []Op{x.Input}
	case *Merge:
		return fmt.Sprintf("Merge(%s)", patternsString([]ast.PatternPart{x.Part})), []Op{x.Input}
	case *SetItems:
		parts := make([]string, len(x.Items))
		for i, it := range x.Items {
			parts[i] = exprString(it.Target) + " = " + exprString(it.Value)
		}
		return fmt.Sprintf("Set(%s)", strings.Join(parts, ", ")), []Op{x.Input}
	case *Delete:
		parts := make([]string, len(x.Exprs))
		for i, e := range x.Exprs {
			parts[i] = exprString(e)
		}
		op := "Delete"
		if x.Detach {
			op = "DetachDelete"
		}
		return fmt.Sprintf("%s(%s)", op, strings.Join(parts, ", ")), []Op{x.Input}
	case *CreateIndex:
		return fmt.Sprintf("CreateIndex(:%s.%s)", x.Label, x.Property), nil
	default:
		return "?", nil
	}
}

// patternsString renders a list of pattern parts compactly for EXPLAIN.
func patternsString(parts []ast.PatternPart) string {
	pieces := make([]string, len(parts))
	for i, p := range parts {
		pieces[i] = patternString(p)
	}
	return strings.Join(pieces, ", ")
}

func patternString(p ast.PatternPart) string {
	s := nodeString(p.Start)
	for _, ch := range p.Chain {
		s += relString(ch.Rel) + nodeString(ch.Node)
	}
	return s
}

func nodeString(n *ast.NodePattern) string {
	var b strings.Builder
	b.WriteByte('(')
	if n.Variable != "" {
		b.WriteString(n.Variable)
	}
	for _, l := range n.Labels {
		b.WriteByte(':')
		b.WriteString(l)
	}
	if len(n.Props) > 0 {
		b.WriteByte(' ')
		b.WriteString(propsString(n.Props))
	}
	b.WriteByte(')')
	return b.String()
}

func relString(r *ast.RelPattern) string {
	var inner string
	if r.Variable != "" {
		inner += r.Variable
	}
	if len(r.Types) > 0 {
		inner += ":" + strings.Join(r.Types, "|")
	}
	if len(r.Props) > 0 {
		if inner != "" {
			inner += " "
		}
		inner += propsString(r.Props)
	}
	body := ""
	if inner != "" {
		body = "[" + inner + "]"
	}
	switch r.Direction {
	case ast.DirOut:
		return "-" + body + "->"
	case ast.DirIn:
		return "<-" + body + "-"
	default:
		return "-" + body + "-"
	}
}

func propsString(props map[string]ast.Expr) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + exprString(props[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
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

// exprString renders an AST expression as readable text.
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
	case *ast.ListLiteral:
		parts := make([]string, len(ex.Elements))
		for i, el := range ex.Elements {
			parts[i] = exprString(el)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *ast.MapLiteral:
		keys := make([]string, 0, len(ex.Entries))
		for k := range ex.Entries {
			keys = append(keys, k)
		}
		sortStrings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + exprString(ex.Entries[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *ast.Case:
		var sb strings.Builder
		sb.WriteString("CASE")
		if ex.Operand != nil {
			sb.WriteByte(' ')
			sb.WriteString(exprString(ex.Operand))
		}
		for _, w := range ex.Whens {
			sb.WriteString(" WHEN ")
			sb.WriteString(exprString(w.Cond))
			sb.WriteString(" THEN ")
			sb.WriteString(exprString(w.Result))
		}
		if ex.Else != nil {
			sb.WriteString(" ELSE ")
			sb.WriteString(exprString(ex.Else))
		}
		sb.WriteString(" END")
		return sb.String()
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
