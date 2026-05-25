package graph

// normalize porta i valori di proprietà alla forma canonica usata internamente:
// gli int Go diventano int64 (come li produce il decode dei record), così record
// e indici vedono lo stesso valore.
func normalize(v any) any {
	if i, ok := v.(int); ok {
		return int64(i)
	}
	return v
}

// isIndexable indica se un valore può finire in un indice `p` (solo scalari;
// list e map non sono indicizzabili in v1, DESIGN §4).
func isIndexable(v any) bool {
	switch v.(type) {
	case nil, bool, int64, float64, string:
		return true
	default:
		return false
	}
}

// scalarEqual confronta due valori scalari per l'uguaglianza (fallback di
// NodesByProperty senza indice).
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
