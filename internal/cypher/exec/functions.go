package exec

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/cypher/ast"
	"github.com/giovibal/intreccio/internal/graph"
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
			return -v, nil
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
