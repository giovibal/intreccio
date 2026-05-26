package mycypher

import (
	"context"
	"strings"
	"testing"
)

// TestPersistenceAcrossOpen writes data through the public API, closes the
// database, reopens it on the same directory and verifies the data is still
// reachable. Exercises BadgerDB's WAL replay path under a normal restart.
func TestPersistenceAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	db1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db1.Query(ctx, "CREATE INDEX FOR (p:Person) ON (p.email)", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.Query(ctx, "CREATE (n:Person {name: 'Alice', email: 'a@b.com'})", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db1.Query(ctx, "CREATE (n:Person {name: 'Bob'})", nil); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()

	res, err := db2.Query(ctx, "MATCH (p:Person) RETURN p.name AS n ORDER BY n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %v, want 2", res.Rows)
	}
	if res.Rows[0][0] != "Alice" || res.Rows[1][0] != "Bob" {
		t.Errorf("rows = %v, want [[Alice] [Bob]]", res.Rows)
	}

	// The index registry must also persist: a lookup-by-email goes through the
	// `p` index after reopen.
	plan, err := db2.Explain(ctx, "MATCH (p:Person) WHERE p.email = $e RETURN p")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "NodeByProperty") {
		t.Errorf("plan after reopen does not use the index:\n%s", plan)
	}

	res, err = db2.Query(ctx, "MATCH (p:Person) WHERE p.email = $e RETURN p.name AS n",
		map[string]any{"e": "a@b.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "Alice" {
		t.Errorf("indexed lookup after reopen = %v, want [[Alice]]", res.Rows)
	}
}
