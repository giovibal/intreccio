package mycypher

import (
	"context"
	"sort"
	"strings"
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

func newDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustQuery(t *testing.T, db *DB, cypher string, params map[string]any) *Result {
	t.Helper()
	res, err := db.Query(context.Background(), cypher, params)
	if err != nil {
		t.Fatalf("Query(%q): %v", cypher, err)
	}
	return res
}

// TestQueryEndToEnd seeds via the graph layer (as before) and reads via Query.
func TestQueryEndToEnd(t *testing.T) {
	db := newDB(t)
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

	res := mustQuery(t, db,
		"MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email = $e RETURN f.name AS name ORDER BY name",
		map[string]any{"e": "a@b.com"})
	if len(res.Columns) != 1 || res.Columns[0] != "name" {
		t.Errorf("columns = %v, want [name]", res.Columns)
	}
	got := stringColumn(res, 0)
	if !equalStrings(got, []string{"Bob", "Carol"}) {
		t.Errorf("rows = %v, want [Bob Carol]", got)
	}
}

func TestCypherCreateAndMatch(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (n:Person {name: 'Bob', age: 30})", nil)
	res := mustQuery(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	if len(res.Rows) != 1 || res.Rows[0][0] != "Bob" {
		t.Errorf("rows = %v, want [[Bob]]", res.Rows)
	}
}

func TestCypherCreateRelationship(t *testing.T) {
	db := newDB(t)
	res := mustQuery(t, db,
		"CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'}) RETURN a.name AS x, b.name AS y", nil)
	if len(res.Rows) != 1 || res.Rows[0][0] != "A" || res.Rows[0][1] != "B" {
		t.Errorf("rows = %v", res.Rows)
	}
	// The relationship is actually persisted.
	count := mustQuery(t, db, "MATCH (x:Person)-[:KNOWS]->(y:Person) RETURN x.name AS n", nil)
	if len(count.Rows) != 1 || count.Rows[0][0] != "A" {
		t.Errorf("traversal rows = %v", count.Rows)
	}
}

func TestMergeIdempotent(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 3; i++ {
		mustQuery(t, db, "MERGE (n:Person {email: $e}) RETURN n.email AS e",
			map[string]any{"e": "a@b.com"})
	}
	res := mustQuery(t, db, "MATCH (p:Person) RETURN p.email AS e", nil)
	if len(res.Rows) != 1 || res.Rows[0][0] != "a@b.com" {
		t.Errorf("expected a single node, got %v", res.Rows)
	}
}

func TestMergeRelationshipBetweenBoundEndpoints(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (a:Person {name: 'A'}), (b:Person {name: 'B'})", nil)
	for i := 0; i < 3; i++ {
		mustQuery(t, db,
			"MATCH (a:Person {name: 'A'}), (b:Person {name: 'B'}) MERGE (a)-[r:KNOWS]->(b) RETURN r", nil)
	}
	res := mustQuery(t, db, "MATCH (x:Person)-[r:KNOWS]->(y:Person) RETURN x.name AS n", nil)
	if len(res.Rows) != 1 {
		t.Errorf("expected a single relationship, got %d rows", len(res.Rows))
	}
}

func TestSetProperty(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (n:Person {name: 'Bob'})", nil)
	mustQuery(t, db, "MATCH (n:Person {name: 'Bob'}) SET n.age = 42", nil)
	res := mustQuery(t, db, "MATCH (n:Person) RETURN n.age AS age", nil)
	if len(res.Rows) != 1 || res.Rows[0][0] != int64(42) {
		t.Errorf("rows = %v", res.Rows)
	}
}

func TestRemoveLabelAndProperty(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (:Person:Admin {name: 'Alice', email: 'a@x'})", nil)
	mustQuery(t, db, "MATCH (n:Person) REMOVE n.email, n:Admin", nil)

	admins := mustQuery(t, db, "MATCH (n:Admin) RETURN n.name AS n", nil)
	if len(admins.Rows) != 0 {
		t.Errorf("expected no admins, got %v", admins.Rows)
	}
	emails := mustQuery(t, db, "MATCH (n:Person) RETURN n.email AS e", nil)
	if len(emails.Rows) != 1 || emails.Rows[0][0] != nil {
		t.Errorf("expected email cleared, got %v", emails.Rows)
	}
}

func TestDeleteEdge(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil)
	mustQuery(t, db, "MATCH (a:Person {name: 'A'})-[r:KNOWS]->(b) DELETE r", nil)
	res := mustQuery(t, db, "MATCH (a:Person)-[r:KNOWS]->(b) RETURN a.name AS n", nil)
	if len(res.Rows) != 0 {
		t.Errorf("expected no relationships, got %v", res.Rows)
	}
	// Both nodes still exist.
	nodes := mustQuery(t, db, "MATCH (p:Person) RETURN p.name AS n", nil)
	if len(nodes.Rows) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(nodes.Rows))
	}
}

func TestDetachDelete(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})", nil)
	mustQuery(t, db, "MATCH (n:Person {name: 'A'}) DETACH DELETE n", nil)

	nodes := mustQuery(t, db, "MATCH (p:Person) RETURN p.name AS n ORDER BY n", nil)
	got := stringColumn(nodes, 0)
	if !equalStrings(got, []string{"B"}) {
		t.Errorf("expected only B, got %v", got)
	}
	rels := mustQuery(t, db, "MATCH (a)-[r:KNOWS]->(b) RETURN a.name AS n", nil)
	if len(rels.Rows) != 0 {
		t.Errorf("expected no leftover edges, got %v", rels.Rows)
	}
}

func TestCreateIndexChangesPlanAndPreservesResults(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// Seed three Person nodes with distinct emails.
	mustQuery(t, db, "CREATE (n:Person {name: 'A', email: 'a@b.com'})", nil)
	mustQuery(t, db, "CREATE (n:Person {name: 'B', email: 'b@b.com'})", nil)
	mustQuery(t, db, "CREATE (n:Person {name: 'C', email: 'c@b.com'})", nil)

	const matchByEmail = "MATCH (p:Person) WHERE p.email = $e RETURN p.name AS n"

	// Before the index: the plan must rely on a label scan.
	before, err := db.Explain(ctx, matchByEmail)
	if err != nil {
		t.Fatalf("Explain before: %v", err)
	}
	if !strings.Contains(before, "NodeByLabelScan") {
		t.Errorf("expected NodeByLabelScan before index, got:\n%s", before)
	}
	if strings.Contains(before, "NodeByProperty") {
		t.Errorf("did NOT expect NodeByProperty before index, got:\n%s", before)
	}

	// Capture pre-index results.
	pre := mustQuery(t, db, matchByEmail, map[string]any{"e": "b@b.com"})
	if len(pre.Rows) != 1 || pre.Rows[0][0] != "B" {
		t.Fatalf("pre-index rows = %v, want [[B]]", pre.Rows)
	}

	// Install the index.
	mustQuery(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)

	// After the index: the plan must use NodeByProperty.
	after, err := db.Explain(ctx, matchByEmail)
	if err != nil {
		t.Fatalf("Explain after: %v", err)
	}
	if !strings.Contains(after, "NodeByProperty") {
		t.Errorf("expected NodeByProperty after index, got:\n%s", after)
	}

	// Backfill check: pre-existing nodes are findable via the index path.
	post := mustQuery(t, db, matchByEmail, map[string]any{"e": "b@b.com"})
	if len(post.Rows) != 1 || post.Rows[0][0] != "B" {
		t.Errorf("post-index rows = %v, want [[B]] (backfill failed?)", post.Rows)
	}
}

func TestCreateIndexIdempotent(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	// A second CREATE INDEX must not error.
	if _, err := db.Query(context.Background(),
		"CREATE INDEX FOR (p:Person) ON (p.email)", nil); err != nil {
		t.Errorf("idempotent CREATE INDEX errored: %v", err)
	}
}

func TestCreateIndexThenInsertUsesIndex(t *testing.T) {
	db := newDB(t)
	// Index before data: new inserts must populate the `p` entries.
	mustQuery(t, db, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	mustQuery(t, db, "CREATE (n:Person {name: 'X', email: 'x@b.com'})", nil)

	res := mustQuery(t, db,
		"MATCH (p:Person) WHERE p.email = $e RETURN p.name AS n",
		map[string]any{"e": "x@b.com"})
	if len(res.Rows) != 1 || res.Rows[0][0] != "X" {
		t.Errorf("rows = %v, want [[X]]", res.Rows)
	}
}

func stringColumn(res *Result, col int) []string {
	out := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		if r[col] == nil {
			continue
		}
		out[i] = r[col].(string)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
