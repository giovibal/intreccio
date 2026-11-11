package intreccio

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCorrelatedLookups covers #24 and #27: a lookup whose value comes from an
// earlier clause must work (inline or in WHERE) and use the index.
func TestCorrelatedLookups(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		db, err := OpenInMemory()
		if err != nil {
			t.Fatal(err)
		}
		run := func(q string, params map[string]any) [][]any {
			t.Helper()
			res, err := db.Query(ctx, q, params)
			if err != nil {
				t.Fatalf("indexed=%v %s: %v", indexed, q, err)
			}
			return res.Rows
		}
		if indexed {
			run("CREATE INDEX FOR (p:Person) ON (p.uid)", nil)
		}
		run("UNWIND [1, 2, 3, 4] AS i CREATE (:Person {uid: i, ref: 5 - i})", nil)

		params := map[string]any{"a": int64(1), "d": []any{int64(2), int64(3), int64(9)}}
		checks := []struct {
			q    string
			want [][]any
		}{
			// The exact query from #24.
			{"MATCH (a:Person {uid: $a}) UNWIND $d AS x MATCH (b:Person {uid: x}) CREATE (a)-[:KNOWS]->(b) RETURN b.uid ORDER BY b.uid",
				[][]any{{int64(2)}, {int64(3)}}},
			{"UNWIND $d AS x MATCH (b:Person) WHERE b.uid = x RETURN b.uid ORDER BY b.uid",
				[][]any{{int64(2)}, {int64(3)}}},
			{"UNWIND $d AS x MATCH (b:Person) WHERE x = b.uid RETURN x ORDER BY x",
				[][]any{{int64(2)}, {int64(3)}}},
			{"MATCH (a:Person {uid: 1}) MATCH (b:Person {uid: a.ref}) RETURN b.uid",
				[][]any{{int64(4)}}},
			{"MATCH (a:Person), (b:Person) WHERE b.uid = a.ref RETURN a.uid, b.uid ORDER BY a.uid",
				[][]any{{int64(1), int64(4)}, {int64(2), int64(3)}, {int64(3), int64(2)}, {int64(4), int64(1)}}},
			{"WITH 1 AS x MATCH (b:Person {uid: x + 1}) RETURN b.uid",
				[][]any{{int64(2)}}},
			{"UNWIND $d AS x OPTIONAL MATCH (b:Person {uid: x}) RETURN x, b.uid ORDER BY x",
				[][]any{{int64(2), int64(2)}, {int64(3), int64(3)}, {int64(9), nil}}},
			{"MATCH (:Person {uid: 1})-[:KNOWS]->(f) RETURN count(f)",
				[][]any{{int64(2)}}},
		}
		for _, c := range checks {
			if got := run(c.q, params); !reflect.DeepEqual(got, c.want) {
				t.Errorf("indexed=%v %s: got %v, want %v", indexed, c.q, got, c.want)
			}
		}
		if indexed {
			plan, err := db.Explain(ctx, "UNWIND $d AS x MATCH (b:Person) WHERE b.uid = x RETURN b")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, "Apply") || !strings.Contains(plan, "NodeByProperty") {
				t.Errorf("expected a per-row index seek, got:\n%s", plan)
			}
		}
		_ = db.Close()
	}
}

// TestCorrelatedLookupScales: a lookup per UNWIND row must cost one index
// seek, not one label scan (#27). On main this query takes about a minute.
func TestCorrelatedLookupScales(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	const n = 20000
	ids := make([]any, n)
	for i := range ids {
		ids[i] = i
	}
	if _, err := db.Query(ctx, "CREATE INDEX FOR (p:Person) ON (p.uid)", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "UNWIND $ids AS i CREATE (:Person {uid: i})", map[string]any{"ids": ids}); err != nil {
		t.Fatal(err)
	}
	dst := ids[:2000]
	start := time.Now()
	res, err := db.Query(ctx,
		"UNWIND $d AS x MATCH (b:Person) WHERE b.uid = x RETURN count(*)",
		map[string]any{"d": dst})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if got := res.Rows[0][0]; got != int64(len(dst)) {
		t.Fatalf("matched %v rows, want %d", got, len(dst))
	}
	// 2000 label scans over 20000 nodes take about a minute; 2000 seeks take
	// tens of milliseconds. The bound is loose to stay stable under -race.
	if elapsed > 5*time.Second {
		t.Errorf("2000 correlated lookups took %v: the index is not used per row", elapsed)
	}
}
