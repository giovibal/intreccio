package intreccio

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestRangeIndexDoesNotChangeResults covers #26: range predicates return the
// same rows with and without an index, across value kinds and bound types.
func TestRangeIndexDoesNotChangeResults(t *testing.T) {
	ctx := context.Background()
	big := int64(1) << 60
	values := []any{
		int64(-5), int64(0), int64(1), int64(2), int64(3), int64(30), int64(31), int64(32), big, big + 1,
		-2.5, math.Copysign(0, -1), 0.0, 0.5, 1.0, 2.5, 30.0, 31.5, float64(big), math.Inf(1), math.Inf(-1), math.NaN(),
		"", "a", "ab", "abc", "abd", "b", "a\x00z", true, false, []any{int64(1)},
	}
	open := func(indexed bool) *DB {
		db, err := OpenInMemory()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if indexed {
			if _, err := db.Query(ctx, "CREATE INDEX FOR (n:P) ON (n.x)", nil); err != nil {
				t.Fatal(err)
			}
		}
		for i, v := range values {
			if _, err := db.Query(ctx, "CREATE (:P {id: $id, x: $x})", map[string]any{"id": i, "x": v}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Query(ctx, "CREATE (:P {id: -1})", nil); err != nil { // no x at all
			t.Fatal(err)
		}
		return db
	}
	plain, indexed := open(false), open(true)

	bounds := []any{
		int64(1), int64(30), int64(-5), big, 1.0, 1.5, 0.0, math.Copysign(0, -1), 30.5,
		math.Inf(1), math.Inf(-1), math.NaN(), "a", "ab", "b", "", true, nil,
	}
	var queries []string
	for _, op := range []string{">", ">=", "<", "<="} {
		queries = append(queries,
			fmt.Sprintf("MATCH (n:P) WHERE n.x %s $a RETURN n.id ORDER BY n.id", op),
			fmt.Sprintf("MATCH (n:P) WHERE $a %s n.x RETURN n.id ORDER BY n.id", op),
		)
	}
	queries = append(queries,
		"MATCH (n:P) WHERE n.x > $a AND n.x < $b RETURN n.id ORDER BY n.id",
		"MATCH (n:P) WHERE n.x >= $a AND n.x <= $b RETURN n.id ORDER BY n.id",
	)
	for _, q := range queries {
		for _, a := range bounds {
			for _, b := range []any{int64(31), 2.5, "abd", math.NaN()} {
				params := map[string]any{"a": a, "b": b}
				want, err := plain.Query(ctx, q, params)
				if err != nil {
					t.Fatalf("%s %v: plain: %v", q, params, err)
				}
				got, err := indexed.Query(ctx, q, params)
				if err != nil {
					t.Fatalf("%s %v: indexed: %v", q, params, err)
				}
				if !reflect.DeepEqual(got.Rows, want.Rows) {
					t.Errorf("%s a=%v b=%v: indexed %v, plain %v", q, a, b, got.Rows, want.Rows)
				}
			}
		}
	}
	for _, prefix := range []any{"a", "ab", "", "a\x00", "zz", nil, int64(1)} {
		q := "MATCH (n:P) WHERE n.x STARTS WITH $p RETURN n.id ORDER BY n.id"
		params := map[string]any{"p": prefix}
		want, err := plain.Query(ctx, q, params)
		if err != nil {
			t.Fatalf("STARTS WITH %q: plain: %v", prefix, err)
		}
		got, err := indexed.Query(ctx, q, params)
		if err != nil {
			t.Fatalf("STARTS WITH %q: indexed: %v", prefix, err)
		}
		if !reflect.DeepEqual(got.Rows, want.Rows) {
			t.Errorf("STARTS WITH %q: indexed %v, plain %v", prefix, got.Rows, want.Rows)
		}
	}
	plan, err := indexed.Explain(ctx, "MATCH (n:P) WHERE n.x > 1 RETURN n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "NodeByPropertyRange") {
		t.Errorf("expected an index range scan:\n%s", plan)
	}
}

// TestRangeScanUpdatesEachNodeOnce: a write query that moves values while it
// scans them (int -> float band) must not see the moved values again.
func TestRangeScanUpdatesEachNodeOnce(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE INDEX FOR (n:P) ON (n.x)", nil); err != nil {
		t.Fatal(err)
	}
	ids := make([]any, 100)
	for i := range ids {
		ids[i] = i + 1
	}
	if _, err := db.Query(ctx, "UNWIND $ids AS i CREATE (:P {x: i, hits: 0})", map[string]any{"ids": ids}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "MATCH (n:P) WHERE n.x > 0 SET n.x = n.x + 1000.5, n.hits = n.hits + 1", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query(ctx, "MATCH (n:P) RETURN min(n.hits), max(n.hits), count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{int64(1), int64(1), int64(100)}}; !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("hits min/max/count = %v, want %v", res.Rows, want)
	}
}

// String predicates are null, not an error, when an operand is not a string
// (openCypher), so a filter over mixed values drops those rows quietly.
func TestStringPredicatesOnNonStrings(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, c := range []struct {
		expr string
		want any
	}{
		{"1 STARTS WITH 'a'", nil},
		{"'abc' STARTS WITH 1", nil},
		{"[1] ENDS WITH 'a'", nil},
		{"true CONTAINS 'a'", nil},
		{"'abc' CONTAINS 'b'", true},
		{"NOT (1 STARTS WITH 'a')", nil},
	} {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", nil)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}
}
