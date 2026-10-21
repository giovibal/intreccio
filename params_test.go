package intreccio

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

type myInt int
type myString string

// TestParameterNormalization covers #21: any reasonable Go type passed as a
// parameter is stored and compared like the equivalent Cypher value.
func TestParameterNormalization(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		in   any
		want any // value read back with RETURN n.v
	}{
		{"int", int(7), int64(7)},
		{"int8", int8(-7), int64(-7)},
		{"int16", int16(7), int64(7)},
		{"int32", int32(7), int64(7)},
		{"int64", int64(7), int64(7)},
		{"uint", uint(7), int64(7)},
		{"uint8", uint8(7), int64(7)},
		{"uint16", uint16(7), int64(7)},
		{"uint32", uint32(7), int64(7)},
		{"uint64", uint64(7), int64(7)},
		{"float32", float32(1.5), 1.5},
		{"float64", 1.5, 1.5},
		{"named int", myInt(7), int64(7)},
		{"named string", myString("s"), "s"},
		{"bool", true, true},
		{"[]int", []int{1, 2}, []any{int64(1), int64(2)}},
		{"[]string", []string{"a", "b"}, []any{"a", "b"}},
		{"[2]float32", [2]float32{1, 2}, []any{1.0, 2.0}},
		{"[][]int32", [][]int32{{1}, {2, 3}}, []any{[]any{int64(1)}, []any{int64(2), int64(3)}}},
		{"[]any", []any{1, "a"}, []any{int64(1), "a"}},
		{"pointer", func() any { v := int32(7); return &v }(), int64(7)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, err := OpenInMemory()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.Query(ctx, "CREATE (:N {v: $v})", map[string]any{"v": c.in}); err != nil {
				t.Fatalf("create: %v", err)
			}
			res, err := db.Query(ctx, "MATCH (n:N) RETURN n.v", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Rows[0][0]; !reflect.DeepEqual(got, c.want) {
				t.Errorf("stored %#v, want %#v", got, c.want)
			}
			// The same parameter must also match in a comparison (scalars only:
			// list equality is covered by #20).
			if reflect.TypeOf(c.want).Kind() == reflect.Slice {
				return
			}
			res, err = db.Query(ctx, "MATCH (n:N) WHERE n.v = $v RETURN count(n)", map[string]any{"v": c.in})
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Rows[0][0]; got != int64(1) {
				t.Errorf("WHERE n.v = $v matched %v nodes, want 1", got)
			}
		})
	}
}

func TestParameterMapNormalization(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.Query(ctx, "RETURN $m AS m", map[string]any{
		"m": map[string]int32{"a": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"a": int64(1)}; !reflect.DeepEqual(res.Rows[0][0], want) {
		t.Errorf("got %#v, want %#v", res.Rows[0][0], want)
	}
}

func TestParameterRejectsUnsupported(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cases := map[string]any{
		"struct":      struct{ A int }{1},
		"uint64 big":  uint64(math.MaxUint64),
		"map int key": map[int]string{1: "a"},
		"func":        func() {},
		"nested":      []any{struct{}{}},
	}
	for name, v := range cases {
		_, err := db.Query(ctx, "RETURN $p", map[string]any{"p": v})
		if err == nil || !strings.Contains(err.Error(), `parameter "p"`) {
			t.Errorf("%s: got err %v, want an error naming parameter \"p\"", name, err)
		}
	}
}
