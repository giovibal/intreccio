package intreccio

import (
	"context"
	"errors"
	"testing"
)

// TestQueryHonorsCanceledContext checks that an already-canceled context stops a
// query before it runs.
func TestQueryHonorsCanceledContext(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (n:Person {name: 'A'})", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := db.Query(ctx, "MATCH (n:Person) RETURN n.name AS name", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestQueryHonorsDeadline checks that an expired deadline stops a query.
func TestQueryHonorsDeadline(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (n:Person {name: 'A'})", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := db.Query(ctx, "MATCH (n:Person) RETURN n.name AS name", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestWithMaxRows checks the result-row cap: at the cap the query succeeds, over
// it fails with ErrResultLimit.
func TestWithMaxRows(t *testing.T) {
	db := newDB(t)
	for _, n := range []string{"A", "B", "C"} {
		mustQuery(t, db, "CREATE (n:Person {name: $n})", map[string]any{"n": n})
	}

	const q = "MATCH (n:Person) RETURN n.name AS name"

	res, err := db.Query(context.Background(), q, nil, WithMaxRows(3))
	if err != nil {
		t.Fatalf("at cap: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("at cap: rows = %d, want 3", len(res.Rows))
	}

	if _, err := db.Query(context.Background(), q, nil, WithMaxRows(2)); !errors.Is(err, ErrResultLimit) {
		t.Fatalf("over cap: err = %v, want ErrResultLimit", err)
	}

	// No cap (and a non-positive cap) returns everything.
	for _, opt := range [][]QueryOption{nil, {WithMaxRows(0)}} {
		res, err := db.Query(context.Background(), q, nil, opt...)
		if err != nil {
			t.Fatalf("no cap: %v", err)
		}
		if len(res.Rows) != 3 {
			t.Fatalf("no cap: rows = %d, want 3", len(res.Rows))
		}
	}
}
