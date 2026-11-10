package intreccio

import (
	"testing"
	"time"
)

// TestBulkRelationshipLoadScales covers #65: loading relationships with index
// seeks costs about the same per row at any batch size. Opening an iterator
// in a write transaction used to sort all its pending writes, so 2 000 rows
// took 6 s and 5 000 rows would take about 40 s; now 5 000 take well under a
// second. The bound is loose enough for the race detector and slow runners.
func TestBulkRelationshipLoadScales(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	db := newDB(t)
	mustQuery(t, db, "CREATE INDEX FOR (n:N) ON (n.id)", nil)
	mustQuery(t, db, "UNWIND range(0, 9999) AS i CREATE (:N {id: i})", nil)
	const rows = 5000
	pairs := make([]any, rows)
	for i := range pairs {
		pairs[i] = map[string]any{"a": i, "b": (i * 7) % 10000}
	}
	start := time.Now()
	mustQuery(t, db, "UNWIND $p AS e MATCH (a:N {id: e.a}), (b:N {id: e.b}) CREATE (a)-[:R]->(b)",
		map[string]any{"p": pairs})
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("loading %d relationships took %v", rows, d)
	}
	if n := countOf(t, db, "MATCH ()-[r:R]->() RETURN count(r)"); n != rows {
		t.Errorf("relationships = %d, want %d", n, rows)
	}
}
