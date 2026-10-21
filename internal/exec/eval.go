package exec

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/query/ast"
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
	case *ast.Subscript:
		return evalSubscript(ex, b, ctx)
	case *ast.Slice:
		return evalSlice(ex, b, ctx)
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
	case map[string]any:
		return t[ex.Key], nil // a missing key is null
	default:
		return nil, fmt.Errorf("exec: property access on a non-node/edge/map value (%T)", target)
	}
}

// evalSubscript evaluates target[index]: a list element by position
// (negative counts from the end, out of range is null) or a map, node or
// relationship property by name.
func evalSubscript(ex *ast.Subscript, b binding, ctx *Context) (any, error) {
	target, err := eval(ex.Target, b, ctx)
	if err != nil {
		return nil, err
	}
	index, err := eval(ex.Index, b, ctx)
	if err != nil {
		return nil, err
	}
	if target == nil || index == nil {
		return nil, nil
	}
	switch t := target.(type) {
	case []any:
		i, ok := normalize(index).(int64)
		if !ok {
			return nil, fmt.Errorf("exec: list index must be an integer, got %T", index)
		}
		if i < 0 {
			i += int64(len(t))
		}
		if i < 0 || i >= int64(len(t)) {
			return nil, nil
		}
		return t[i], nil
	case map[string]any, graph.Node, graph.Edge:
		key, ok := index.(string)
		if !ok {
			return nil, fmt.Errorf("exec: map key must be a string, got %T", index)
		}
		switch m := t.(type) {
		case map[string]any:
			return m[key], nil
		case graph.Node:
			return propValue(ctx, m.Props, key)
		default:
			return propValue(ctx, m.(graph.Edge).Props, key)
		}
	default:
		return nil, fmt.Errorf("exec: cannot index a %T", target)
	}
}

// evalSlice evaluates list[from..to]: bounds are clamped to the list,
// negative bounds count from the end, and a null list or bound gives null.
func evalSlice(ex *ast.Slice, b binding, ctx *Context) (any, error) {
	target, err := eval(ex.Target, b, ctx)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, nil
	}
	list, ok := target.([]any)
	if !ok {
		return nil, fmt.Errorf("exec: cannot slice a %T", target)
	}
	n := int64(len(list))
	bound := func(e ast.Expr, open int64) (int64, bool, error) {
		if e == nil {
			return open, true, nil
		}
		v, err := eval(e, b, ctx)
		if err != nil || v == nil {
			return 0, false, err
		}
		i, ok := normalize(v).(int64)
		if !ok {
			return 0, false, fmt.Errorf("exec: slice bound must be an integer, got %T", v)
		}
		if i < 0 {
			i += n
		}
		return min(max(i, 0), n), true, nil
	}
	from, ok, err := bound(ex.From, 0)
	if err != nil || !ok {
		return nil, err
	}
	to, ok, err := bound(ex.To, n)
	if err != nil || !ok {
		return nil, err
	}
	if from >= to {
		return []any{}, nil
	}
	return append([]any(nil), list[from:to]...), nil
}

func propValue(ctx *Context, props map[uint32]any, key string) (any, error) {
	keyID, found, err := ctx.lookupKey(key)
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
		id, found, err := ctx.lookupLabel(name)
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
			return negateInt(n)
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
		return boolOp(ex, b, ctx, false)
	case "OR":
		return boolOp(ex, b, ctx, true)
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
		return equalityOp(l, r, false), nil
	case "<>":
		return equalityOp(l, r, true), nil
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

// stringPredicate evaluates STARTS WITH, ENDS WITH and CONTAINS. As in
// openCypher, the result is null unless both operands are strings.
func stringPredicate(l, r any, fn func(string, string) bool) (any, error) {
	ls, lok := l.(string)
	rs, rok := r.(string)
	if !lok || !rok {
		return nil, nil
	}
	return fn(ls, rs), nil
}

// inList evaluates `needle IN haystack` with three-valued logic: true if an
// element equals needle, otherwise null if any comparison was unknown (a null
// needle or a null element), otherwise false.
func inList(needle, haystack any) (any, error) {
	if haystack == nil {
		return nil, nil
	}
	list, ok := haystack.([]any)
	if !ok {
		return nil, fmt.Errorf("exec: IN requires a list, got %T", haystack)
	}
	unknown := false
	for _, item := range list {
		eq, known := equality(needle, item)
		if known && eq {
			return true, nil
		}
		if !known {
			unknown = true
		}
	}
	if unknown {
		return nil, nil
	}
	return false, nil
}

// boolOp evaluates AND (or OR when isOr is set) with three-valued logic: the
// deciding value (false for AND, true for OR) wins over null; otherwise null
// makes the result null. Non-boolean operands count as false, as before.
func boolOp(ex *ast.Binary, b binding, ctx *Context, isOr bool) (any, error) {
	l, err := eval(ex.Left, b, ctx)
	if err != nil {
		return nil, err
	}
	r, err := eval(ex.Right, b, ctx)
	if err != nil {
		return nil, err
	}
	if (l != nil && truthy(l) == isOr) || (r != nil && truthy(r) == isOr) {
		return isOr, nil
	}
	if l == nil || r == nil {
		return nil, nil
	}
	return !isOr, nil
}

// equals reports whether a and b are definitely equal. An unknown result
// (null involved) counts as not equal, which is what IN, CASE and MERGE need.
func equals(a, b any) bool {
	eq, known := equality(a, b)
	return known && eq
}

// equalityOp evaluates `=` (or `<>` when negate is set) with openCypher
// three-valued logic: the result is null when it depends on a null.
func equalityOp(a, b any, negate bool) any {
	eq, known := equality(a, b)
	if !known {
		return nil
	}
	return eq != negate
}

// equality compares two values. known is false when the outcome depends on a
// null (e.g. 1 = null, or [1, null] = [1, null]). Lists compare element by
// element, maps key by key, nodes and relationships by identity, and values of
// different kinds are unequal.
func equality(a, b any) (eq, known bool) {
	a, b = normalize(a), normalize(b)
	if a == nil || b == nil {
		return false, false
	}
	switch av := a.(type) {
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false, true
		}
		known = true
		for i := range av {
			e, k := equality(av[i], bv[i])
			if k && !e {
				return false, true
			}
			known = known && k
		}
		return known, known
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false, true
		}
		known = true
		for key, x := range av {
			y, present := bv[key]
			if !present {
				return false, true
			}
			e, k := equality(x, y)
			if k && !e {
				return false, true
			}
			known = known && k
		}
		return known, known
	case graph.Node:
		bv, ok := b.(graph.Node)
		return ok && av.ID == bv.ID, true
	case graph.Edge:
		bv, ok := b.(graph.Edge)
		return ok && av.ID == bv.ID, true
	}
	// Scalars: shared with the graph layer so an index lookup and a filter
	// agree (1 = 1.0, NaN equals nothing, different kinds are unequal).
	return graph.QueryEqual(a, b), true
}

// compareOp evaluates <, <=, > and >= with openCypher semantics: null when
// either side is null or the values cannot be compared (e.g. 1 < 'a'), and
// false whenever NaN is involved. ORDER BY, min and max keep using
// compareValues, which they need as an ordering.
func compareOp(op string, a, b any) any {
	a, b = normalize(a), normalize(b)
	if a == nil || b == nil {
		return nil
	}
	if isNaN(a) || isNaN(b) {
		return false
	}
	c, ok := compareValues(a, b)
	if !ok {
		return nil
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
		return nil
	}
}

func isNaN(v any) bool {
	f, ok := v.(float64)
	return ok && math.IsNaN(f)
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

// ErrIntegerOverflow is returned when integer arithmetic leaves the int64
// range. openCypher integers do not wrap around.
var ErrIntegerOverflow = errors.New("exec: integer overflow")

func intArithmetic(op string, a, b int64) (any, error) {
	switch op {
	case "+":
		return addInt(a, b)
	case "-":
		c := a - b
		if (a^b)&(a^c) < 0 {
			return nil, ErrIntegerOverflow
		}
		return c, nil
	case "*":
		if a == 0 || b == 0 {
			return int64(0), nil
		}
		c := a * b
		if c/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
			return nil, ErrIntegerOverflow
		}
		return c, nil
	case "/":
		if b == 0 {
			return nil, fmt.Errorf("exec: division by zero")
		}
		if a == math.MinInt64 && b == -1 {
			return nil, ErrIntegerOverflow
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

// addInt adds two int64 values, failing instead of wrapping around.
func addInt(a, b int64) (int64, error) {
	c := a + b
	if (a^c)&(b^c) < 0 {
		return 0, ErrIntegerOverflow
	}
	return c, nil
}

// negateInt negates an int64; -MinInt64 does not fit.
func negateInt(n int64) (int64, error) {
	if n == math.MinInt64 {
		return 0, ErrIntegerOverflow
	}
	return -n, nil
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
