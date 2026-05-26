package graph

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

// scalarEqual compares two scalar values for equality (fallback of
// NodesByProperty without an index).
func scalarEqual(a, b any) bool {
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
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	default:
		return false
	}
}
