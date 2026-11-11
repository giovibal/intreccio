package intreccio

import (
	"context"
	"reflect"
	"testing"
)

// These tests cover live queries against the features that reach the store
// through their own paths or change what a result can depend on.

// TestSubscribeSeesRelationshipPropertyChanges: relationships are not
// immutable, so a property set on one must reach both kinds of live query.
func TestSubscribeSeesRelationshipPropertyChanges(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:P {n: 'a'})-[:K {w: 1}]->(:P {n: 'b'})")

	anchored := subscribe(t, db, "MATCH (a:P)-[r:K]->(b:P) RETURN a.n AS a, r.w AS w, b.n AS b", nil)
	whole := subscribe(t, db, "MATCH ()-[r:K]->() RETURN r", nil)
	total := subscribe(t, db, "MATCH ()-[r:K]->() RETURN sum(r.w) AS s", nil) // re-run path
	nextChange(t, anchored)
	nextChange(t, whole)
	nextChange(t, total)
	before := db.liveStats.anchored.Load()

	mustExec(t, db, "MATCH ()-[r:K]->() SET r.w = 5")
	wantChange(t, nextChange(t, anchored), [][]any{{"a", int64(5), "b"}}, [][]any{{"a", int64(1), "b"}})
	c := nextChange(t, whole)
	if len(c.Added) != 1 || len(c.Removed) != 1 ||
		c.Added[0][0].(Relationship).Props["w"] != int64(5) || c.Removed[0][0].(Relationship).Props["w"] != int64(1) {
		t.Errorf("whole relationship: %+v", c)
	}
	wantChange(t, nextChange(t, total), oneCol(int64(5)), oneCol(int64(1)))
	if db.liveStats.anchored.Load() == before {
		t.Error("the relationship update was not handled by an anchored evaluation")
	}

	mustExec(t, db, "MATCH ()-[r:K]->() REMOVE r.w")
	wantChange(t, nextChange(t, anchored), [][]any{{"a", nil, "b"}}, [][]any{{"a", int64(5), "b"}})

	// A relationship of another type is nobody's business here.
	mustExec(t, db, "MATCH (a:P {n: 'a'}), (b:P {n: 'b'}) CREATE (a)-[:OTHER {w: 9}]->(b)")
	mustExec(t, db, "MATCH ()-[r:OTHER]->() SET r.w = 10")
	expectQuiet(t, anchored)
}

// TestSubscribeSeesTransactions: a multi-statement transaction is one commit,
// so it is one batch; one that fails is none.
func TestSubscribeSeesTransactions(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	sub := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	nextChange(t, sub)

	err := db.Update(ctx, func(tx *Tx) error {
		for _, name := range []string{"Ann", "Bob", "Cy"} {
			if _, err := tx.Query(ctx, "CREATE (:Person {name: $n})", map[string]any{"n": name}); err != nil {
				return err
			}
		}
		_, err := tx.Query(ctx, "MATCH (p:Person {name: 'Bob'}) DELETE p", nil)
		return err
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	c := nextChange(t, sub)
	if len(c.Added) != 2 || len(c.Removed) != 0 {
		t.Fatalf("one transaction, one batch with its net effect: got %+v", c)
	}
	expectQuiet(t, sub)

	boom := context.Canceled
	if err := db.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Query(ctx, "CREATE (:Person {name: 'Ghost'})", nil); err != nil {
			return err
		}
		return boom
	}); err != boom {
		t.Fatalf("rolled-back Update: err = %v", err)
	}
	expectQuiet(t, sub)

	// A read-only transaction is not a commit.
	if err := db.View(ctx, func(tx *Tx) error {
		_, err := tx.Query(ctx, "MATCH (p:Person) RETURN p", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expectQuiet(t, sub)
}

// TestSubscribeSeesImport: every batch of an Import is a commit of its own.
func TestSubscribeSeesImport(t *testing.T) {
	db := newDB(t)
	sub := subscribe(t, db, "MATCH (p:Person) RETURN count(p) AS c", nil)
	nextChange(t, sub)

	rows := make([]map[string]any, 25)
	for i := range rows {
		rows[i] = map[string]any{"name": i}
	}
	res, err := db.Import(context.Background(), "UNWIND $rows AS r CREATE (p:Person) SET p = r", rows, nil, WithBatchSize(10))
	if err != nil || res.Rows != 25 || res.Batches != 3 {
		t.Fatalf("Import: %+v, %v", res, err)
	}
	var last any
	for last != int64(25) {
		c := nextChange(t, sub)
		if len(c.Added) != 1 {
			t.Fatalf("batch = %+v", c)
		}
		last = c.Added[0][0]
	}
	expectQuiet(t, sub)
}

// TestSubscribeFollowsPaths: a path carries nodes the pattern does not name, so
// a change to any of them changes the result.
func TestSubscribeFollowsPaths(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:City {n: 'a'})-[:ROAD]->(:Town {n: 'm'})-[:ROAD]->(:City {n: 'z'})")
	sub := subscribe(t, db,
		"MATCH (a:City {n: 'a'}), (z:City {n: 'z'}), p = shortestPath((a)-[:ROAD*]-(z)) RETURN p", nil)
	first := nextChange(t, sub)
	if len(first.Added) != 1 {
		t.Fatalf("initial = %+v", first)
	}
	if p := first.Added[0][0].(Path); len(p.Nodes) != 3 || p.Nodes[1].Props["n"] != "m" {
		t.Fatalf("path = %v", first.Added[0][0])
	}

	// The middle node is a Town: no label the query mentions.
	mustExec(t, db, "MATCH (t:Town) SET t.n = 'mid'")
	c := nextChange(t, sub)
	if len(c.Added) != 1 || len(c.Removed) != 1 || c.Added[0][0].(Path).Nodes[1].Props["n"] != "mid" {
		t.Fatalf("after renaming a node on the path: %+v", c)
	}

	// A shortcut makes a shorter path.
	mustExec(t, db, "MATCH (a:City {n: 'a'}), (z:City {n: 'z'}) CREATE (a)-[:ROAD]->(z)")
	c = nextChange(t, sub)
	if len(c.Added) != 1 || len(c.Added[0][0].(Path).Nodes) != 2 {
		t.Fatalf("after adding a shortcut: %+v", c)
	}
}

// TestSubscribeRejectsCatalogListings: SHOW changes without any node or
// relationship changing, so it cannot be followed live.
func TestSubscribeRejectsCatalogListings(t *testing.T) {
	db := newDB(t)
	for _, q := range []string{"SHOW INDEXES", "STREAM SHOW CONSTRAINTS"} {
		if sub, err := db.Subscribe(context.Background(), q, nil); err == nil {
			_ = sub.Close()
			t.Errorf("Subscribe(%q): expected an error", q)
		}
	}
	// Schema changes touch no record: a live query is not woken by them...
	sub := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	nextChange(t, sub)
	mustExec(t, db, "CREATE INDEX FOR (p:Person) ON (p.name)")
	mustExec(t, db, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.email IS UNIQUE")
	expectQuiet(t, sub)
	// ...and keeps working on top of them.
	mustExec(t, db, "CREATE (:Person {name: 'Ann', email: 'a@x'})")
	wantChange(t, nextChange(t, sub), oneCol("Ann"), nil)
}

// TestSubscribeWithEntityParameter: a node passed back as a parameter names the
// stored node, in a live query as in a plain one.
func TestSubscribeWithEntityParameter(t *testing.T) {
	db := newDB(t)
	res, err := db.Query(context.Background(), "CREATE (a:P {n: 'a'})-[:K]->(:P {n: 'b'}) RETURN a", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Rows[0][0].(Node)
	sub := subscribe(t, db, "MATCH (x)-[:K]->(y) WHERE x = $a RETURN y.n AS n", map[string]any{"a": a})
	if first := nextChange(t, sub); !reflect.DeepEqual(first.Added, oneCol("b")) {
		t.Fatalf("initial = %+v", first)
	}
	mustExec(t, db, "MATCH (x:P {n: 'a'}) CREATE (x)-[:K]->(:P {n: 'c'})")
	wantChange(t, nextChange(t, sub), oneCol("c"), nil)
}
