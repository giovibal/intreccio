package exec

import (
	"fmt"
	"strings"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/cypher/ast"
	"github.com/giovibal/intreccio/internal/graph"
)

// binding is one row of execution: variable/column name -> value.
// Node variables hold a graph.Node, relationship variables a graph.Edge, and
// everything else a scalar (nil, bool, int64, float64, string).
type binding map[string]any

func (b binding) clone() binding {
	out := make(binding, len(b))
	for k, v := range b {
		out[k] = v
	}
	return out
}

// eval evaluates an expression against a row.
func eval(e ast.Expr, b binding, ctx *Context) (any, error) {
	switch ex := e.(type) {
	case *ast.Literal:
		return ex.Value, nil
	case *ast.Param:
		v, ok := ctx.Params[ex.Name]
		if !ok {
			return nil, fmt.Errorf("exec: parameter $%s not provided", ex.Name)
		}
		return normalize(v), nil
	case *ast.Variable:
		v, ok := b[ex.Name]
		if !ok {
			return nil, fmt.Errorf("exec: unbound variable %s", ex.Name)
		}
		return v, nil
	case *ast.PropertyAccess:
		return evalProperty(ex, b, ctx)
	case *ast.Unary:
		return evalUnary(ex, b, ctx)
	case *ast.Binary:
		return evalBinary(ex, b, ctx)
	case *ast.LabelsPredicate:
		return evalLabels(ex, b, ctx)
	case *ast.ListLiteral:
		out := make([]any, len(ex.Elements))
		for i, el := range ex.Elements {
			v, err := eval(el, b, ctx)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case *ast.MapLiteral:
		out := make(map[string]any, len(ex.Entries))
		for k, v := range ex.Entries {
			val, err := eval(v, b, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = val
		}
		return out, nil
	case *ast.Case:
		return evalCase(ex, b, ctx)
	case *ast.FunctionCall:
		return evalFunctionCall(ex, b, ctx)
	default:
		return nil, fmt.Errorf("exec: unsupported expression %T", e)
	}
}

func evalCase(ex *ast.Case, b binding, ctx *Context) (any, error) {
	if ex.Operand != nil {
		operand, err := eval(ex.Operand, b, ctx)
		if err != nil {
			return nil, err
		}
		for _, w := range ex.Whens {
			candidate, err := eval(w.Cond, b, ctx)
			if err != nil {
				return nil, err
			}
			if equals(operand, candidate) {
				return eval(w.Result, b, ctx)
			}
		}
	} else {
		for _, w := range ex.Whens {
			cond, err := eval(w.Cond, b, ctx)
			if err != nil {
				return nil, err
			}
			if truthy(cond) {
				return eval(w.Result, b, ctx)
			}
		}
	}
	if ex.Else != nil {
		return eval(ex.Else, b, ctx)
	}
	return nil, nil
}

func evalProperty(ex *ast.PropertyAccess, b binding, ctx *Context) (any, error) {
	target, err := eval(ex.Target, b, ctx)
	if err != nil {
		return nil, err
	}
	switch t := target.(type) {
	case nil:
		return nil, nil
	case graph.Node:
		return propValue(ctx, t.Props, ex.Key)
	case graph.Edge:
		return propValue(ctx, t.Props, ex.Key)
	default:
		return nil, fmt.Errorf("exec: property access on a non-node/edge value (%T)", target)
	}
}

func propValue(ctx *Context, props map[uint32]any, key string) (any, error) {
	keyID, found, err := catalog.LookupKey(ctx.Txn, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return props[keyID], nil
}

func evalLabels(ex *ast.LabelsPredicate, b binding, ctx *Context) (any, error) {
	target, err := eval(ex.Expr, b, ctx)
	if err != nil {
		return nil, err
	}
	node, ok := target.(graph.Node)
	if !ok {
		return false, nil
	}
	for _, name := range ex.Labels {
		id, found, err := catalog.LookupLabel(ctx.Txn, name)
		if err != nil {
			return nil, err
		}
		if !found || !hasLabel(node, id) {
			return false, nil
		}
	}
	return true, nil
}

func hasLabel(n graph.Node, labelID uint32) bool {
	for _, l := range n.Labels {
		if l == labelID {
			return true
		}
	}
	return false
}

func evalUnary(ex *ast.Unary, b binding, ctx *Context) (any, error) {
	v, err := eval(ex.Expr, b, ctx)
	if err != nil {
		return nil, err
	}
	switch ex.Op {
	case "NOT":
		if v == nil {
			return nil, nil
		}
		bv, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("exec: NOT requires a boolean, got %T", v)
		}
		return !bv, nil
	case "-":
		switch n := v.(type) {
		case int64:
			return -n, nil
		case float64:
			return -n, nil
		default:
			return nil, fmt.Errorf("exec: unary minus requires a number, got %T", v)
		}
	case "IS NULL":
		return v == nil, nil
	case "IS NOT NULL":
		return v != nil, nil
	default:
		return nil, fmt.Errorf("exec: unknown unary operator %q", ex.Op)
	}
}

func evalBinary(ex *ast.Binary, b binding, ctx *Context) (any, error) {
	switch ex.Op {
	case "AND":
		return boolOp(ex, b, ctx, func(l, r bool) bool { return l && r })
	case "OR":
		return boolOp(ex, b, ctx, func(l, r bool) bool { return l || r })
	}

	l, err := eval(ex.Left, b, ctx)
	if err != nil {
		return nil, err
	}
	r, err := eval(ex.Right, b, ctx)
	if err != nil {
		return nil, err
	}

	switch ex.Op {
	case "=":
		return equals(l, r), nil
	case "<>":
		return notEquals(l, r), nil
	case "<", "<=", ">", ">=":
		return compareOp(ex.Op, l, r), nil
	case "+", "-", "*", "/", "%":
		return arithmetic(ex.Op, l, r)
	case "STARTS WITH":
		return stringPredicate(l, r, strings.HasPrefix)
	case "ENDS WITH":
		return stringPredicate(l, r, strings.HasSuffix)
	case "CONTAINS":
		return stringPredicate(l, r, strings.Contains)
	case "IN":
		return inList(l, r)
	default:
		return nil, fmt.Errorf("exec: unknown binary operator %q", ex.Op)
	}
}

func stringPredicate(l, r any, fn func(string, string) bool) (any, error) {
	if l == nil || r == nil {
		return nil, nil
	}
	ls, ok := l.(string)
	if !ok {
		return nil, fmt.Errorf("exec: string predicate left operand must be a string, got %T", l)
	}
	rs, ok := r.(string)
	if !ok {
		return nil, fmt.Errorf("exec: string predicate right operand must be a string, got %T", r)
	}
	return fn(ls, rs), nil
}

func inList(needle, haystack any) (any, error) {
	if haystack == nil {
		return nil, nil
	}
	list, ok := haystack.([]any)
	if !ok {
		return nil, fmt.Errorf("exec: IN requires a list, got %T", haystack)
	}
	for _, item := range list {
		if equals(needle, item) {
			return true, nil
		}
	}
	return false, nil
}

func boolOp(ex *ast.Binary, b binding, ctx *Context, f func(l, r bool) bool) (any, error) {
	l, err := eval(ex.Left, b, ctx)
	if err != nil {
		return nil, err
	}
	r, err := eval(ex.Right, b, ctx)
	if err != nil {
		return nil, err
	}
	return f(truthy(l), truthy(r)), nil
}

func equals(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	c, ok := compareValues(a, b)
	return ok && c == 0
}

func notEquals(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	c, ok := compareValues(a, b)
	return ok && c != 0
}

func compareOp(op string, a, b any) bool {
	c, ok := compareValues(a, b)
	if !ok {
		return false
	}
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	default:
		return false
	}
}

// compareValues compares two scalar values, returning the sign of a-b and whether
// they are comparable.
func compareValues(a, b any) (int, bool) {
	a, b = normalize(a), normalize(b)
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			return cmpInt64(av, bv), true
		case float64:
			return cmpFloat64(float64(av), bv), true
		}
	case float64:
		switch bv := b.(type) {
		case int64:
			return cmpFloat64(av, float64(bv)), true
		case float64:
			return cmpFloat64(av, bv), true
		}
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv), true
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return cmpBool(av, bv), true
		}
	}
	return 0, false
}

func arithmetic(op string, a, b any) (any, error) {
	a, b = normalize(a), normalize(b)
	if op == "+" {
		if as, ok := a.(string); ok {
			if bs, ok := b.(string); ok {
				return as + bs, nil
			}
		}
	}
	ai, aIsInt := a.(int64)
	bi, bIsInt := b.(int64)
	if aIsInt && bIsInt {
		return intArithmetic(op, ai, bi)
	}
	af, ok := toFloat(a)
	if !ok {
		return nil, fmt.Errorf("exec: arithmetic requires numbers, got %T", a)
	}
	bf, ok := toFloat(b)
	if !ok {
		return nil, fmt.Errorf("exec: arithmetic requires numbers, got %T", b)
	}
	return floatArithmetic(op, af, bf)
}

func intArithmetic(op string, a, b int64) (any, error) {
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	case "/":
		if b == 0 {
			return nil, fmt.Errorf("exec: division by zero")
		}
		return a / b, nil
	case "%":
		if b == 0 {
			return nil, fmt.Errorf("exec: division by zero")
		}
		return a % b, nil
	default:
		return nil, fmt.Errorf("exec: unknown operator %q", op)
	}
}

func floatArithmetic(op string, a, b float64) (any, error) {
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "*":
		return a * b, nil
	case "/":
		if b == 0 {
			return nil, fmt.Errorf("exec: division by zero")
		}
		return a / b, nil
	case "%":
		return nil, fmt.Errorf("exec: modulo requires integers")
	default:
		return nil, fmt.Errorf("exec: unknown operator %q", op)
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// truthy reports whether a value counts as true (only a boolean true does;
// null and non-booleans are false).
func truthy(v any) bool {
	bv, ok := v.(bool)
	return ok && bv
}

// normalize converts Go ints to int64 so values from parameters/literals compare
// consistently with stored values.
func normalize(v any) any {
	if i, ok := v.(int); ok {
		return int64(i)
	}
	return v
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}
