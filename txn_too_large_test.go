package intreccio

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestTxnTooLargeIsRecognisable covers #29: a write that exceeds the engine's
// per-transaction limit fails with ErrTxnTooLarge, commits nothing, and the
// same data loads fine in batches.
func TestTxnTooLargeIsRecognisable(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("INTRECCIO_TEST_ENGINE") == "bolt" {
		t.Skip("bbolt has no per-transaction size limit")
	}
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	const total = 40000 // above the ~26k-35k nodes one statement can hold
	ids := make([]any, total)
	for i := range ids {
		ids[i] = i
	}
	const q = "UNWIND $ids AS i CREATE (:Big {uid: i})"

	_, err = db.Query(ctx, q, map[string]any{"ids": ids})
	if !errors.Is(err, ErrTxnTooLarge) {
		t.Fatalf("got err %v, want ErrTxnTooLarge", err)
	}
	res, err := db.Query(ctx, "MATCH (n:Big) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(0) {
		t.Fatalf("a failed write must not commit anything, found %v nodes", got)
	}

	// The documented remedy: the same data in batches of 10 000 rows.
	const batch = 10000
	for start := 0; start < total; start += batch {
		if _, err := db.Query(ctx, q, map[string]any{"ids": ids[start : start+batch]}); err != nil {
			t.Fatalf("batch at %d: %v", start, err)
		}
	}
	res, err = db.Query(ctx, "MATCH (n:Big) RETURN count(n)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(total) {
		t.Fatalf("batched load: count %v, want %d", got, total)
	}
}
