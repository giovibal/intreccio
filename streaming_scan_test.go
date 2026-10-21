package intreccio

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestStreamingScansAreClosed covers #25: streaming scans keep a storage
// iterator open, and the engine panics if a transaction ends with one still
// open. Every way a query can stop early must close them.
func TestStreamingScansAreClosed(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ids := make([]any, 500)
	for i := range ids {
		ids[i] = i
	}
	if _, err := db.Query(ctx, "UNWIND $ids AS i CREATE (:L {i: i})", map[string]any{"ids": ids}); err != nil {
		t.Fatal(err)
	}

	ok := []struct {
		cypher string
		want   [][]any
	}{
		{"MATCH (n:L) RETURN n.i LIMIT 2", [][]any{{int64(0)}, {int64(1)}}},
		{"MATCH (n) RETURN count(*) AS c", [][]any{{int64(500)}}},
		// Writes in the same (read-write) transaction as a stopped scan.
		{"MATCH (n:L) WITH n LIMIT 2 SET n.seen = true RETURN n.i", [][]any{{int64(0)}, {int64(1)}}},
		{"MATCH (n:L) WITH n LIMIT 1 CREATE (:M) RETURN n.i", [][]any{{int64(0)}}},
		{"UNWIND [1, 2] AS x OPTIONAL MATCH (n:L) RETURN x, n.i LIMIT 1", [][]any{{int64(1), int64(0)}}},
		{"MATCH (a:L), (b:L) RETURN a.i, b.i LIMIT 1", [][]any{{int64(0), int64(0)}}},
		{"MATCH (n:Nope) RETURN n", nil},
	}
	for _, c := range ok {
		res, err := db.Query(ctx, c.cypher, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.cypher, err)
		}
		if !reflect.DeepEqual(res.Rows, c.want) {
			t.Errorf("%s: got %v, want %v", c.cypher, res.Rows, c.want)
		}
	}

	// Errors in the middle of a scan, in read and write transactions.
	for _, q := range []string{
		"MATCH (n:L) RETURN 1 / (n.i - 3)",
		"MATCH (n:L) SET n.x = 1 / (n.i - 3)",
	} {
		if _, err := db.Query(ctx, q, nil); err == nil {
			t.Errorf("%s: expected a division-by-zero error", q)
		}
	}
	// Row cap and cancellation stop a scan too.
	if _, err := db.Query(ctx, "MATCH (n:L) RETURN n.i", nil, WithMaxRows(5)); !errors.Is(err, ErrResultLimit) {
		t.Errorf("WithMaxRows: got %v, want ErrResultLimit", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Query(cctx, "MATCH (n:L) RETURN n.i", nil); err == nil {
		t.Error("cancelled context: expected an error")
	}
	// The database is still usable afterwards.
	res, err := db.Query(ctx, "MATCH (n:L) WHERE n.seen RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(2) {
		t.Errorf("seen = %v, want 2", got)
	}
}
