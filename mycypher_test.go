package mycypher

import (
	"context"
	"sort"
	"testing"

	"github.com/giovibal/mycypher/internal/graph"
	"github.com/giovibal/mycypher/internal/storage"
)

func TestOpenClose(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestQueryEndToEnd(t *testing.T) {
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Seed via the graph layer (the Cypher write path arrives in Phase 7).
	if err := db.store.Update(func(tx storage.Txn) error {
		alice, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Alice", "email": "a@b.com"})
		if err != nil {
			return err
		}
		bob, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Bob"})
		if err != nil {
			return err
		}
		carol, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Carol"})
		if err != nil {
			return err
		}
		for _, e := range [][2]uint64{{alice, bob}, {alice, carol}} {
			if _, err := graph.CreateEdge(tx, "KNOWS", e[0], e[1], nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := db.Query(context.Background(),
		"MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email = $e RETURN f.name AS name ORDER BY name",
		map[string]any{"e": "a@b.com"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Columns) != 1 || res.Columns[0] != "name" {
		t.Errorf("columns = %v, want [name]", res.Columns)
	}

	got := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		got[i] = r[0].(string)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "Bob" || got[1] != "Carol" {
		t.Errorf("rows = %v, want [Bob Carol]", got)
	}
}

func TestQueryWriteUnsupported(t *testing.T) {
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Query(context.Background(), "CREATE (n:Person)", nil); err == nil {
		t.Error("expected an error for a write query (Phase 7)")
	}
}
