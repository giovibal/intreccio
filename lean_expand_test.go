package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// TestAnonymousElementsGiveSameResults covers #28: skipping the records of
// anonymous pattern elements must not change any result.
func TestAnonymousElementsGiveSameResults(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	setup := `CREATE (a:P {name: 'a'}), (b:P {name: 'b'}), (c:Q {name: 'c'}),
		(a)-[:R {w: 1}]->(b), (a)-[:R {w: 2}]->(c), (b)-[:R {w: 1}]->(c), (a)-[:R {w: 3}]->(a)`
	if _, err := db.Query(ctx, setup, nil); err != nil {
		t.Fatal(err)
	}
	pairs := []struct{ anon, named string }{
		{"MATCH (n:P)-[:R]->() RETURN n.name, count(*) AS d ORDER BY n.name",
			"MATCH (n:P)-[r:R]->(m) RETURN n.name, count(*) AS d ORDER BY n.name"},
		{"MATCH (n:P)-[:R]->(:Q) RETURN n.name ORDER BY n.name",
			"MATCH (n:P)-[r:R]->(m:Q) RETURN n.name ORDER BY n.name"},
		{"MATCH (n:P)-[:R {w: 1}]->() RETURN n.name ORDER BY n.name",
			"MATCH (n:P)-[r:R]->(m) WHERE r.w = 1 RETURN n.name ORDER BY n.name"},
		{"MATCH (n:P)<-[:R]-() RETURN n.name, count(*) ORDER BY n.name",
			"MATCH (n:P)<-[r:R]-(m) RETURN n.name, count(*) ORDER BY n.name"},
		{"MATCH (n:P)-[:R]-() RETURN n.name, count(*) ORDER BY n.name",
			"MATCH (n:P)-[r:R]-(m) RETURN n.name, count(*) ORDER BY n.name"},
		{"MATCH (n:P)-[:R]->(n) RETURN n.name",
			"MATCH (n:P)-[r:R]->(m) WHERE m = n RETURN n.name"},
		{"MATCH (n:P) OPTIONAL MATCH (n)-[:R]->(:Q) RETURN n.name, count(*) ORDER BY n.name",
			"MATCH (n:P) OPTIONAL MATCH (n)-[r:R]->(m:Q) RETURN n.name, count(*) ORDER BY n.name"},
		{"MATCH (n:P)-[:R]->() WITH * RETURN n.name ORDER BY n.name",
			"MATCH (n:P)-[r:R]->(m) RETURN n.name ORDER BY n.name"},
	}
	for _, p := range pairs {
		got, err := db.Query(ctx, p.anon, nil)
		if err != nil {
			t.Fatalf("%s: %v", p.anon, err)
		}
		want, err := db.Query(ctx, p.named, nil)
		if err != nil {
			t.Fatalf("%s: %v", p.named, err)
		}
		if !reflect.DeepEqual(got.Rows, want.Rows) {
			t.Errorf("%s: %v\nnamed variant: %v", p.anon, got.Rows, want.Rows)
		}
	}
}

// TestLookupCacheSeesKeysCreatedByTheQuery: a property name looked up before
// it exists and created later in the same query must then be found.
func TestLookupCacheSeesKeysCreatedByTheQuery(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Query(ctx, "CREATE (:C), (:C)", nil); err != nil {
		t.Fatal(err)
	}
	res, err := db.Query(ctx, "MATCH (n:C) WHERE n.fresh IS NULL SET n.fresh = 7, n:Fresh RETURN n.fresh, n:Fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{int64(7), true}, {int64(7), true}}; !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("got %v, want %v", res.Rows, want)
	}
}
