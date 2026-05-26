package exec

import (
	"fmt"
	"strings"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/graph"
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
	case *ast.FunctionCall:
		return nil, fmt.Errorf("exec: function %s not supported yet", ex.Name)
	default:
		return nil, fmt.Errorf("exec: unsupported expression %T", e)
	}
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
	default:
		return nil, fmt.Errorf("exec: unknown binary operator %q", ex.Op)
	}
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
