package exec

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/query/ast"
)

// scalarFunc is the signature of all built-in scalar functions. It takes the
// execution context (used for catalog lookups) and the evaluated argument list.
type scalarFunc func(ctx *Context, args []any) (any, error)

// scalarFuncs is the registry of supported scalar (non-aggregate) functions.
// Functions are looked up case-insensitively.
var scalarFuncs = map[string]scalarFunc{
	"id":         fnID,
	"labels":     fnLabels,
	"type":       fnType,
	"keys":       fnKeys,
	"properties": fnProperties,
	"size":       fnSize,
	"length":     fnLength,
	"tointeger":  fnToInteger,
	"tofloat":    fnToFloat,
	"tostring":   fnToString,
	"toupper":    fnToUpper,
	"tolower":    fnToLower,
	"trim":       fnTrim,
	"substring":  fnSubstring,
	"replace":    fnReplace,
	"split":      fnSplit,
	"abs":        fnAbs,
	"head":       fnHead,
	"last":       fnLast,
	"tail":       fnTail,
	"range":      fnRange,
	"coalesce":   fnCoalesce,
	"startnode":  fnStartNode,
	"endnode":    fnEndNode,
	"round":      fnRound,
	"floor":      fnFloor,
	"ceil":       fnCeil,
	"sqrt":       fnSqrt,
	"sign":       fnSign,
	"rand":       fnRand,
	"timestamp":  fnTimestamp,
}

// maxRangeLen bounds range() so a typo cannot exhaust memory.
const maxRangeLen = 10_000_000

// fnRange returns the integers from start to end inclusive, by step.
func fnRange(_ *Context, args []any) (any, error) {
	if err := needArgs("range", args, 2, 3); err != nil {
		return nil, err
	}
	nums := make([]int64, len(args))
	for i, a := range args {
		n, ok := normalize(a).(int64)
		if !ok {
			return nil, fmt.Errorf("exec: range() expects integers, got %T", a)
		}
		nums[i] = n
	}
	start, end, step := nums[0], nums[1], int64(1)
	if len(nums) == 3 {
		step = nums[2]
	}
	if step == 0 {
		return nil, fmt.Errorf("exec: range() step must not be zero")
	}
	// Size the result up front (in float64, which cannot overflow here) so an
	// oversized range fails before allocating anything.
	if n := (float64(end)-float64(start))/float64(step) + 1; n > maxRangeLen {
		return nil, fmt.Errorf("exec: range() would produce more than %d elements", maxRangeLen)
	}
	out := []any{}
	for i := start; (step > 0 && i <= end) || (step < 0 && i >= end); i += step {
		out = append(out, i)
		if (step > 0 && i > math.MaxInt64-step) || (step < 0 && i < math.MinInt64-step) {
			break // the next step would overflow
		}
	}
	return out, nil
}

// fnCoalesce returns its first non-null argument.
func fnCoalesce(_ *Context, args []any) (any, error) {
	if err := needArgs("coalesce", args, 1, -1); err != nil {
		return nil, err
	}
	for _, a := range args {
		if a != nil {
			return a, nil
		}
	}
	return nil, nil
}

func edgeEnd(name string, ctx *Context, args []any, start bool) (any, error) {
	if err := needArgs(name, args, 1, 1); err != nil {
		return nil, err
	}
	switch e := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Edge:
		id := e.Dst
		if start {
			id = e.Src
		}
		return graph.GetNode(ctx.Txn, id)
	default:
		return nil, fmt.Errorf("exec: %s() expects a relationship, got %T", name, args[0])
	}
}

func fnStartNode(ctx *Context, args []any) (any, error) { return edgeEnd("startNode", ctx, args, true) }
func fnEndNode(ctx *Context, args []any) (any, error)   { return edgeEnd("endNode", ctx, args, false) }

// numericFunc applies f to a numeric argument and returns a float, as the
// openCypher math functions do; null gives null.
func numericFunc(name string, args []any, f func(float64) float64) (any, error) {
	if err := needArgs(name, args, 1, 1); err != nil {
		return nil, err
	}
	switch v := normalize(args[0]).(type) {
	case nil:
		return nil, nil
	case int64:
		return f(float64(v)), nil
	case float64:
		return f(v), nil
	default:
		return nil, fmt.Errorf("exec: %s() expects a number, got %T", name, args[0])
	}
}

// fnRound rounds half up (towards positive infinity), as openCypher does.
func fnRound(_ *Context, args []any) (any, error) {
	return numericFunc("round", args, func(x float64) float64 { return math.Floor(x + 0.5) })
}
func fnFloor(_ *Context, args []any) (any, error) { return numericFunc("floor", args, math.Floor) }
func fnCeil(_ *Context, args []any) (any, error)  { return numericFunc("ceil", args, math.Ceil) }
func fnSqrt(_ *Context, args []any) (any, error)  { return numericFunc("sqrt", args, math.Sqrt) }

// fnSign returns -1, 0 or 1 as an integer.
func fnSign(_ *Context, args []any) (any, error) {
	if err := needArgs("sign", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := normalize(args[0]).(type) {
	case nil:
		return nil, nil
	case int64:
		return int64(cmpInt64(v, 0)), nil
	case float64:
		switch {
		case v > 0:
			return int64(1), nil
		case v < 0:
			return int64(-1), nil
		}
		return int64(0), nil
	default:
		return nil, fmt.Errorf("exec: sign() expects a number, got %T", args[0])
	}
}

func fnRand(_ *Context, args []any) (any, error) {
	if err := needArgs("rand", args, 0, 0); err != nil {
		return nil, err
	}
	return rand.Float64(), nil
}

// fnTimestamp returns milliseconds since the epoch, the same value for every
// call within one query.
func fnTimestamp(ctx *Context, args []any) (any, error) {
	if err := needArgs("timestamp", args, 0, 0); err != nil {
		return nil, err
	}
	return ctx.timestamp(), nil
}

func evalFunctionCall(fc *ast.FunctionCall, b binding, ctx *Context) (any, error) {
	if isAggregateName(fc.Name) {
		return nil, fmt.Errorf("exec: aggregate %s used outside aggregation context", fc.Name)
	}
	fn, ok := scalarFuncs[strings.ToLower(fc.Name)]
	if !ok {
		return nil, fmt.Errorf("exec: unknown function %s", fc.Name)
	}
	args := make([]any, len(fc.Args))
	for i, a := range fc.Args {
		v, err := eval(a, b, ctx)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(ctx, args)
}

func isAggregateName(name string) bool {
	switch strings.ToLower(name) {
	case "count", "sum", "avg", "min", "max", "collect":
		return true
	}
	return false
}

func needArgs(name string, args []any, min, max int) error {
	if len(args) < min || (max >= 0 && len(args) > max) {
		if min == max {
			return fmt.Errorf("exec: %s expects %d argument(s), got %d", name, min, len(args))
		}
		return fmt.Errorf("exec: %s expects %d..%d arguments, got %d", name, min, max, len(args))
	}
	return nil
}

// --- identity / introspection ---

func fnID(_ *Context, args []any) (any, error) {
	if err := needArgs("id", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Node:
		return int64(v.ID), nil
	case graph.Edge:
		return int64(v.ID), nil
	default:
		return nil, fmt.Errorf("exec: id() expects a node or relationship, got %T", v)
	}
}

func fnLabels(ctx *Context, args []any) (any, error) {
	if err := needArgs("labels", args, 1, 1); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	n, ok := args[0].(graph.Node)
	if !ok {
		return nil, fmt.Errorf("exec: labels() expects a node, got %T", args[0])
	}
	out := make([]any, 0, len(n.Labels))
	for _, id := range n.Labels {
		name, err := catalog.LabelName(ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

func fnType(ctx *Context, args []any) (any, error) {
	if err := needArgs("type", args, 1, 1); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	e, ok := args[0].(graph.Edge)
	if !ok {
		return nil, fmt.Errorf("exec: type() expects a relationship, got %T", args[0])
	}
	return catalog.TypeName(ctx.Txn, e.Type)
}

func fnKeys(ctx *Context, args []any) (any, error) {
	if err := needArgs("keys", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Node:
		return keyNames(ctx, v.Props)
	case graph.Edge:
		return keyNames(ctx, v.Props)
	case map[string]any:
		out := make([]any, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("exec: keys() expects a node, relationship or map, got %T", v)
	}
}

func keyNames(ctx *Context, props map[uint32]any) ([]any, error) {
	out := make([]any, 0, len(props))
	for id := range props {
		name, err := catalog.KeyName(ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

func fnProperties(ctx *Context, args []any) (any, error) {
	if err := needArgs("properties", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case graph.Node:
		return propsMap(ctx, v.Props)
	case graph.Edge:
		return propsMap(ctx, v.Props)
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("exec: properties() expects a node, relationship or map, got %T", v)
	}
}

func propsMap(ctx *Context, props map[uint32]any) (map[string]any, error) {
	out := make(map[string]any, len(props))
	for id, v := range props {
		name, err := catalog.KeyName(ctx.Txn, id)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

// --- size / length ---

func fnSize(_ *Context, args []any) (any, error) {
	if err := needArgs("size", args, 1, 1); err != nil {
		return nil, err
	}
	return sizeOf("size", args[0])
}

func fnLength(_ *Context, args []any) (any, error) {
	if err := needArgs("length", args, 1, 1); err != nil {
		return nil, err
	}
	return sizeOf("length", args[0])
}

func sizeOf(name string, v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return int64(len([]rune(x))), nil
	case []any:
		return int64(len(x)), nil
	case []graph.Edge:
		return int64(len(x)), nil
	default:
		return nil, fmt.Errorf("exec: %s() expects a list or string, got %T", name, v)
	}
}

// --- conversions ---

func fnToInteger(_ *Context, args []any) (any, error) {
	if err := needArgs("toInteger", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case int64:
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, nil
		}
		return int64(v), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, nil
		}
		return n, nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	default:
		return nil, fmt.Errorf("exec: toInteger() unsupported type %T", v)
	}
}

func fnToFloat(_ *Context, args []any) (any, error) {
	if err := needArgs("toFloat", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, nil
		}
		return f, nil
	default:
		return nil, fmt.Errorf("exec: toFloat() unsupported type %T", v)
	}
}

func fnToString(_ *Context, args []any) (any, error) {
	if err := needArgs("toString", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case string:
		return v, nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// --- string functions ---

func fnToUpper(_ *Context, args []any) (any, error) {
	if err := needArgs("toUpper", args, 1, 1); err != nil {
		return nil, err
	}
	return stringFn(args[0], strings.ToUpper)
}

func fnToLower(_ *Context, args []any) (any, error) {
	if err := needArgs("toLower", args, 1, 1); err != nil {
		return nil, err
	}
	return stringFn(args[0], strings.ToLower)
}

func fnTrim(_ *Context, args []any) (any, error) {
	if err := needArgs("trim", args, 1, 1); err != nil {
		return nil, err
	}
	return stringFn(args[0], strings.TrimSpace)
}

func stringFn(v any, fn func(string) string) (any, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("exec: expected a string, got %T", v)
	}
	return fn(s), nil
}

// fnSubstring is substring(s, start [, length]); start and length are
// zero-based character (rune) offsets.
func fnSubstring(_ *Context, args []any) (any, error) {
	if err := needArgs("substring", args, 2, 3); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("exec: substring() expects a string, got %T", args[0])
	}
	startIdx, err := intArg("substring", args[1])
	if err != nil {
		return nil, err
	}
	runes := []rune(s)
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx > int64(len(runes)) {
		return "", nil
	}
	if len(args) == 2 {
		return string(runes[startIdx:]), nil
	}
	length, err := intArg("substring", args[2])
	if err != nil {
		return nil, err
	}
	end := startIdx + length
	if end < startIdx {
		end = startIdx
	}
	if end > int64(len(runes)) {
		end = int64(len(runes))
	}
	return string(runes[startIdx:end]), nil
}

func fnReplace(_ *Context, args []any) (any, error) {
	if err := needArgs("replace", args, 3, 3); err != nil {
		return nil, err
	}
	if args[0] == nil || args[1] == nil || args[2] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("exec: replace() expects a string, got %T", args[0])
	}
	from, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("exec: replace() expects a string for `search`, got %T", args[1])
	}
	to, ok := args[2].(string)
	if !ok {
		return nil, fmt.Errorf("exec: replace() expects a string for `replacement`, got %T", args[2])
	}
	return strings.ReplaceAll(s, from, to), nil
}

func fnSplit(_ *Context, args []any) (any, error) {
	if err := needArgs("split", args, 2, 2); err != nil {
		return nil, err
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("exec: split() expects a string, got %T", args[0])
	}
	sep, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("exec: split() expects a string for `delimiter`, got %T", args[1])
	}
	parts := strings.Split(s, sep)
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out, nil
}

// --- math ---

func fnAbs(_ *Context, args []any) (any, error) {
	if err := needArgs("abs", args, 1, 1); err != nil {
		return nil, err
	}
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case int64:
		if v < 0 {
			return negateInt(v)
		}
		return v, nil
	case float64:
		return math.Abs(v), nil
	default:
		return nil, fmt.Errorf("exec: abs() expects a number, got %T", v)
	}
}

// --- list helpers ---

func fnHead(_ *Context, args []any) (any, error) {
	if err := needArgs("head", args, 1, 1); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	list, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("exec: head() expects a list, got %T", args[0])
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[0], nil
}

func fnLast(_ *Context, args []any) (any, error) {
	if err := needArgs("last", args, 1, 1); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	list, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("exec: last() expects a list, got %T", args[0])
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[len(list)-1], nil
}

func fnTail(_ *Context, args []any) (any, error) {
	if err := needArgs("tail", args, 1, 1); err != nil {
		return nil, err
	}
	if args[0] == nil {
		return nil, nil
	}
	list, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("exec: tail() expects a list, got %T", args[0])
	}
	if len(list) == 0 {
		return []any{}, nil
	}
	out := make([]any, len(list)-1)
	copy(out, list[1:])
	return out, nil
}

func intArg(name string, v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case float64:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("exec: %s() expects an integer, got %T", name, v)
	}
}
