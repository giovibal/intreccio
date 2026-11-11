package graph

import "math"

// normalize brings property values to the canonical form used internally:
// Go ints become int64 (as produced by record decoding), so records and indexes
// see the same value.
func normalize(v any) any {
	if i, ok := v.(int); ok {
		return int64(i)
	}
	return v
}

// IsIndexable reports whether a value can go into a `p` index (scalars only;
// lists and maps are not indexable in v1, DESIGN §4).
func IsIndexable(v any) bool {
	switch v.(type) {
	case nil, bool, int64, float64, string:
		return true
	default:
		return false
	}
}

// isIndexable is the package-internal alias kept for the existing callers.
func isIndexable(v any) bool { return IsIndexable(v) }

// sameValue reports whether two scalars are the same stored value: same type
// and, for floats, the same bits. It decides whether an index entry must be
// rewritten, so it must distinguish values with different `p` keys (e.g. 0.0
// and -0.0, or 1 and 1.0) even when a query would consider them equal.
func sameValue(a, b any) bool {
	a, b = normalize(a), normalize(b)
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case string:
		y, ok := b.(string)
		return ok && x == y
	default:
		return false
	}
}

// QueryEqual is the openCypher equality used by queries on scalar values:
// integers and floats compare numerically (1 = 1.0, 0.0 = -0.0), NaN equals
// nothing, and null equals nothing. Values of different kinds are never equal.
// Index lookups must return exactly the nodes this function accepts.
func QueryEqual(a, b any) bool {
	a, b = normalize(a), normalize(b)
	switch x := a.(type) {
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case float64:
			return float64(x) == y
		}
	case float64:
		switch y := b.(type) {
		case int64:
			return x == float64(y)
		case float64:
			return x == y
		}
	}
	return false
}

// maxExactInt is 2^53: below this magnitude each integral float64 equals
// exactly one int64.
const maxExactInt = 1 << 53

// equalityProbes returns the stored values whose `p` keys an index lookup for v
// must visit to find every node that QueryEqual accepts. ok is false when the
// set cannot be enumerated as a few exact keys (a float beyond 2^53 equals
// several integers); the caller then falls back to a label scan.
func equalityProbes(v any) (probes []any, ok bool) {
	switch x := v.(type) {
	case int64:
		f := float64(x)
		if f == 0 {
			return []any{x, f, math.Copysign(0, -1)}, true
		}
		return []any{x, f}, true
	case float64:
		switch {
		case math.IsNaN(x):
			return nil, true
		case x == 0:
			return []any{int64(0), float64(0), math.Copysign(0, -1)}, true
		case math.IsInf(x, 0):
			return []any{x}, true
		case math.Abs(x) >= maxExactInt:
			return nil, false
		case x == math.Trunc(x):
			return []any{x, int64(x)}, true
		default:
			return []any{x}, true
		}
	default:
		return []any{v}, true
	}
}
