package intreccio

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func people(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"uid": i, "name": "p"}
	}
	return out
}

// TestImport covers #29: rows are written in batches of separate transactions.
func TestImport(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	res, err := db.Import(ctx, "UNWIND $rows AS r CREATE (p:Person) SET p = r, p.src = $src",
		people(25_000), map[string]any{"src": "csv"}, WithBatchSize(10_000))
	if err != nil {
		t.Fatal(err)
	}
	if res != (ImportResult{Rows: 25_000, Batches: 3}) {
		t.Errorf("result = %+v", res)
	}
	if got := rowsOf(t, db, "MATCH (p:Person {src: 'csv'}) RETURN count(p), min(p.uid), max(p.uid)"); got != "[[25000 0 24999]]" {
		t.Errorf("loaded = %s", got)
	}

	// Relationships, finding their endpoints through an index, from scalar
	// rows of any slice type.
	mustQuery(t, db, "CREATE INDEX FOR (p:Person) ON (p.uid)", nil)
	ids := make([]int, 20_000)
	for i := range ids {
		ids[i] = i
	}
	res, err = db.Import(ctx, `UNWIND $rows AS i
		MATCH (a:Person {uid: i}), (b:Person {uid: i + 1}) CREATE (a)-[:NEXT]->(b)`, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != (ImportResult{Rows: 20_000, Batches: 2}) {
		t.Errorf("relationships result = %+v", res)
	}
	if got := rowsOf(t, db, "MATCH ()-[r:NEXT]->() RETURN count(r)"); got != "[[20000]]" {
		t.Errorf("relationships = %s", got)
	}

	// Nothing to import.
	if res, err := db.Import(ctx, "UNWIND $rows AS r CREATE (:X)", []any{}, nil); err != nil || res != (ImportResult{}) {
		t.Errorf("empty import = %+v, %v", res, err)
	}
}

// TestImportShrinksOversizedBatches: a batch over the transaction limit is
// retried in halves and the import completes.
func TestImportShrinksOversizedBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("INTRECCIO_TEST_ENGINE") == "bolt" {
		t.Skip("bbolt has no per-transaction size limit")
	}
	ctx := context.Background()
	db := newDB(t)
	const n = 80_000 // over the ~50k nodes one transaction holds
	res, err := db.Import(ctx, "UNWIND $rows AS r CREATE (p:Big) SET p = r", people(n), nil, WithBatchSize(n))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != n || res.Batches < 2 {
		t.Errorf("result = %+v, want %d rows in at least 2 batches", res, n)
	}
	if got := rowsOf(t, db, "MATCH (p:Big) RETURN count(p)"); got != "[[80000]]" {
		t.Errorf("loaded = %s", got)
	}
}

// TestImportStopsAtFailedBatch: earlier batches stay, the error names the
// failed rows, and the result says where to resume.
func TestImportStopsAtFailedBatch(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	mustQuery(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.uid IS UNIQUE", nil)
	rows := people(1000)
	rows[650]["uid"] = 10 // duplicates row 10, in the 3rd batch of 300
	res, err := db.Import(ctx, "UNWIND $rows AS r CREATE (p:Person) SET p = r", rows, nil, WithBatchSize(300))
	if !errors.Is(err, ErrConstraintViolation) || !strings.Contains(err.Error(), "rows 600 to 899") {
		t.Fatalf("err = %v", err)
	}
	if res != (ImportResult{Rows: 600, Batches: 2}) {
		t.Errorf("result = %+v", res)
	}
	if got := rowsOf(t, db, "MATCH (p:Person) RETURN count(p)"); got != "[[600]]" {
		t.Errorf("committed = %s", got)
	}
	// Resume after fixing the row.
	rows[650]["uid"] = 650
	res, err = db.Import(ctx, "UNWIND $rows AS r CREATE (p:Person) SET p = r", rows[600:], nil, WithBatchSize(300))
	if err != nil || res.Rows != 400 {
		t.Fatalf("resume = %+v, %v", res, err)
	}

	// A cancelled context stops before the next batch.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if res, err := db.Import(cctx, "UNWIND $rows AS r CREATE (:Y)", []int{1, 2}, nil); !errors.Is(err, context.Canceled) || res.Rows != 0 {
		t.Errorf("cancelled = %+v, %v", res, err)
	}
}

func TestImportRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	for _, tc := range []struct {
		q      string
		rows   any
		params map[string]any
		want   string
	}{
		{"UNWIND $rows AS r CREATE (:X)", 5, nil, "must be a slice"},
		{"UNWIND $rows AS r CREATE (:X)", []int{1}, map[string]any{"rows": 1}, `must not contain "rows"`},
		{"UNWIND $rows AS r RETURN r", []int{1}, nil, "must write"},
		{"CREATE (:X {v: $v})", []int{1}, map[string]any{"v": 1}, "$rows"},
		{"UNWIND $rows AS r CREATE (:X", []int{1}, nil, "expected"},
	} {
		_, err := db.Import(ctx, tc.q, tc.rows, tc.params)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.q, err, tc.want)
		}
	}
	if got := rowsOf(t, db, "MATCH (x:X) RETURN count(x)"); got != "[[0]]" {
		t.Errorf("rejected imports wrote %s", got)
	}
}
