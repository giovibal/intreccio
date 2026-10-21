package ast

import (
	"fmt"
	"sort"
	"strings"
)

// Format renders an expression as Cypher-like text. It names projection
// columns that have no alias (RETURN a.b -> "a.b"), so the analyzer and the
// planner must both use it to agree on column names.
func Format(e Expr) string {
	switch ex := e.(type) {
	case nil:
		return ""
	case *Variable:
		return ex.Name
	case *PropertyAccess:
		return Format(ex.Target) + "." + ex.Key
	case *Param:
		return "$" + ex.Name
	case *Literal:
		return formatLiteral(ex.Value)
	case *Unary:
		if ex.Op == "NOT" {
			return "NOT " + Format(ex.Expr)
		}
		return ex.Op + Format(ex.Expr)
	case *Binary:
		return Format(ex.Left) + " " + ex.Op + " " + Format(ex.Right)
	case *LabelsPredicate:
		return Format(ex.Expr) + ":" + strings.Join(ex.Labels, ":")
	case *ListLiteral:
		parts := make([]string, len(ex.Elements))
		for i, el := range ex.Elements {
			parts[i] = Format(el)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *MapLiteral:
		keys := make([]string, 0, len(ex.Entries))
		for k := range ex.Entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + Format(ex.Entries[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *Case:
		var sb strings.Builder
		sb.WriteString("CASE")
		if ex.Operand != nil {
			sb.WriteByte(' ')
			sb.WriteString(Format(ex.Operand))
		}
		for _, w := range ex.Whens {
			sb.WriteString(" WHEN ")
			sb.WriteString(Format(w.Cond))
			sb.WriteString(" THEN ")
			sb.WriteString(Format(w.Result))
		}
		if ex.Else != nil {
			sb.WriteString(" ELSE ")
			sb.WriteString(Format(ex.Else))
		}
		sb.WriteString(" END")
		return sb.String()
	case *FunctionCall:
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
			sb.WriteString(Format(a))
		}
		sb.WriteByte(')')
		return sb.String()
	case *Subscript:
		return Format(ex.Target) + "[" + Format(ex.Index) + "]"
	case *Slice:
		return Format(ex.Target) + "[" + Format(ex.From) + ".." + Format(ex.To) + "]"
	default:
		return "?"
	}
}

func formatLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "'" + x + "'"
	default:
		return fmt.Sprintf("%v", x)
	}
}
