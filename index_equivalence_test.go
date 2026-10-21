package intreccio

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// TestIndexDoesNotChangeEqualityResults runs the same equality queries against
// two databases holding the same data, one with a property index and one
// without. An index is an access path only: the rows must be identical.
func TestIndexDoesNotChangeEqualityResults(t *testing.T) {
	ctx := context.Background()
	values := []any{
		int64(1), 1.0, 1.5, int64(0), 0.0, math.Copysign(0, -1), math.NaN(),
		math.Inf(1), "1", "a", true, false, int64(1) << 60, int64(1)<<60 + 1, float64(int64(1) << 60),
	}
	probes := append([]any{int64(2), 2.0, "b"}, values...)

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
				t.Fatalf("create %v: %v", v, err)
			}
		}
		return db
	}
	plain, indexed := open(false), open(true)

	queries := []string{
		"MATCH (n:P {x: $v}) RETURN n.id ORDER BY n.id",
		"MATCH (n:P) WHERE n.x = $v RETURN n.id ORDER BY n.id",
	}
	for _, q := range queries {
		for _, v := range probes {
			name := fmt.Sprintf("%s/%v(%T)", q, v, v)
			want, err := plain.Query(ctx, q, map[string]any{"v": v})
			if err != nil {
				t.Fatalf("%s: plain: %v", name, err)
			}
			got, err := indexed.Query(ctx, q, map[string]any{"v": v})
			if err != nil {
				t.Fatalf("%s: indexed: %v", name, err)
			}
			if !reflect.DeepEqual(got.Rows, want.Rows) {
				t.Errorf("%s: indexed rows %v, plain rows %v", name, got.Rows, want.Rows)
			}
		}
	}

	// Spot-check the semantics, not just the agreement.
	res, err := indexed.Query(ctx, "MATCH (n:P) WHERE n.x = 1 RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(2) {
		t.Errorf("x = 1 should match both 1 and 1.0, got count %v", got)
	}
	res, err = indexed.Query(ctx, "MATCH (n:P) WHERE n.x = 0 RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(3) {
		t.Errorf("x = 0 should match 0, 0.0 and -0.0, got count %v", got)
	}
}

// TestMergeDoesNotMatchNaN: NaN is unequal to every value, so MERGE on a NaN
// property must not match an existing numeric node.
func TestMergeDoesNotMatchNaN(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:M {x: 1})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "MERGE (:M {x: $v})", map[string]any{"v": math.NaN()}); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query(ctx, "MATCH (n:M) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(2) {
		t.Errorf("MERGE with NaN matched an existing node: count %v, want 2", got)
	}
}
