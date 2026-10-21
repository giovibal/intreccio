package intreccio

import (
	"fmt"
	"math"
	"reflect"

	"github.com/giovibal/intreccio/internal/graph"
)

// normalizeParams converts query parameters to the value types the engine
// works with: every integer type becomes int64, float32 becomes float64, slices
// and arrays become []any and string-keyed maps become map[string]any
// (recursively), and non-nil pointers are dereferenced. Named types (e.g.
// `type ID int`) are converted by their underlying kind. Values that have no
// Cypher equivalent are rejected with an error naming the parameter.
func normalizeParams(params map[string]any) (map[string]any, error) {
	if len(params) == 0 {
		return params, nil
	}
	out := make(map[string]any, len(params))
	for name, v := range params {
		nv, err := normalizeParam(v)
		if err != nil {
			return nil, fmt.Errorf("query: parameter %q: %w", name, err)
		}
		out[name] = nv
	}
	return out, nil
}

func normalizeParam(v any) (any, error) {
	switch x := v.(type) {
	// Fast paths for the types the engine already uses.
	case nil, bool, int64, float64, string:
		return v, nil
	case int:
		return int64(x), nil
	case graph.Node, graph.Edge:
		return v, nil // a node or relationship taken from a previous result
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, fmt.Errorf("unsigned value %d overflows a 64-bit integer", u)
		}
		return int64(u), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return normalizeParam(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil, nil
		}
		list := make([]any, rv.Len())
		for i := range list {
			item, err := normalizeParam(rv.Index(i).Interface())
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			list[i] = item
		}
		return list, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported map key type %s (keys must be strings)", rv.Type().Key())
		}
		if rv.IsNil() {
			return nil, nil
		}
		m := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			item, err := normalizeParam(iter.Value().Interface())
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			m[key] = item
		}
		return m, nil
	}
	return nil, fmt.Errorf("unsupported type %T", v)
}
