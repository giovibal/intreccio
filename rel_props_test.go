package intreccio

import (
	"context"
	"strings"
	"testing"
)

// TestSetRelationshipProperties covers #67: SET and REMOVE on relationship
// properties, alone or mixed with node updates in one clause.
func TestSetRelationshipProperties(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE (:P {name: 'a'})-[:KNOWS {w: 1}]->(:P {name: 'b'})", nil)
	props := func() string {
		t.Helper()
		return rowsOf(t, db, "MATCH ()-[r:KNOWS]->() RETURN properties(r)")
	}
	steps := []struct{ q, want string }{
		{"MATCH ()-[r:KNOWS]->() SET r.w = 2", "[[map[w:2]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r.since = 2020, r.w = r.w + 1", "[[map[since:2020 w:3]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r += {x: 'y', since: null}", "[[map[w:3 x:y]]]"},
		{"MATCH ()-[r:KNOWS]->() REMOVE r.x", "[[map[w:3]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r.w = null", "[[map[]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r = {a: 1, b: [1, 2]}", "[[map[a:1 b:[1 2]]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r += null", "[[map[a:1 b:[1 2]]]]"},
		{"MATCH ()-[r:KNOWS]->() SET r = null", "[[map[]]]"},
		// a relationship and its endpoint in the same SET clause
		{"MATCH (a)-[r:KNOWS]->() SET r.w = 5, a.seen = true, r.v = 6", "[[map[v:6 w:5]]]"},
	}
	for _, s := range steps {
		mustQuery(t, db, s.q, nil)
		if got := props(); got != s.want {
			t.Fatalf("after %s: properties = %s, want %s", s.q, got, s.want)
		}
	}
	if got := rowsOf(t, db, "MATCH (a:P {name: 'a'}) RETURN a.seen"); got != "[[true]]" {
		t.Errorf("node SET in the same clause: %s", got)
	}
	// Type and endpoints are unchanged, and the bound variable sees the update.
	if got := rowsOf(t, db, "MATCH (a)-[r:KNOWS]->(b) SET r.w = 7 RETURN a.name, type(r), r.w, b.name"); got != "[[a KNOWS 7 b]]" {
		t.Errorf("after SET: %s", got)
	}
	if got := rowsOf(t, db, "MATCH (b {name: 'b'})<-[r:KNOWS]-(a) RETURN a.name, r.w"); got != "[[a 7]]" {
		t.Errorf("incoming view: %s", got)
	}

	// MERGE ON CREATE / ON MATCH on a relationship.
	merge := "MATCH (a {name: 'a'}), (b {name: 'b'}) MERGE (a)-[r:LIKES]->(b) ON CREATE SET r.n = 1 ON MATCH SET r.n = r.n + 1 RETURN r.n"
	for _, want := range []string{"[[1]]", "[[2]]", "[[3]]"} {
		if got := rowsOf(t, db, merge); got != want {
			t.Errorf("MERGE = %s, want %s", got, want)
		}
	}

	// SET and REMOVE on a null relationship do nothing.
	mustQuery(t, db, "MATCH (b {name: 'b'}) OPTIONAL MATCH (b)-[r:NONE]->() SET r.w = 1, r += {x: 1} REMOVE r.w", nil)

	// Relationships have no labels.
	if _, err := db.Query(context.Background(), "MATCH ()-[r:KNOWS]->() SET r:Label", nil); err == nil ||
		!strings.Contains(err.Error(), "only nodes have labels") {
		t.Errorf("SET r:Label = %v", err)
	}
}

// TestSetRelationshipPropertiesKeepsIndex: after every kind of change, a seek
// on the relationship index returns what a filter over all relationships
// returns.
func TestSetRelationshipPropertiesKeepsIndex(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE INDEX FOR ()-[r:T]-() ON (r.w)", nil)
	mustQuery(t, db, "UNWIND range(0, 19) AS i CREATE (:N {i: i})-[:T {id: i, w: i % 4}]->(:N)", nil)
	check := func(stage string) {
		t.Helper()
		for _, v := range []string{"0", "1", "2", "3", "1.0", "'x'", "[1]"} {
			seek := "MATCH ()-[r:T {w: " + v + "}]->() RETURN r.id ORDER BY r.id"
			scan := "MATCH ()-[r:T]->() WHERE [r.w][0] = " + v + " RETURN r.id ORDER BY r.id"
			plan, _ := db.Explain(context.Background(), seek)
			if !strings.Contains(plan, "RelByProperty") {
				t.Fatalf("%s: %s does not seek:\n%s", stage, seek, plan)
			}
			if plan, _ := db.Explain(context.Background(), scan); strings.Contains(plan, "RelByProperty") {
				t.Fatalf("%s: the reference query %s uses the index:\n%s", stage, scan, plan)
			}
			if a, b := rowsOf(t, db, seek), rowsOf(t, db, scan); a != b {
				t.Errorf("%s: w = %s: index %s, scan %s", stage, v, a, b)
			}
		}
	}
	check("initial")
	for _, q := range []string{
		"MATCH ()-[r:T]->() WHERE r.id < 5 SET r.w = r.w + 1",
		"MATCH ()-[r:T]->() WHERE r.id = 6 SET r.w = 1.0",
		"MATCH ()-[r:T]->() WHERE r.id = 7 SET r.w = 'x'",
		"MATCH ()-[r:T]->() WHERE r.id = 8 SET r.w = [1]",
		"MATCH ()-[r:T]->() WHERE r.id = 9 REMOVE r.w",
		"MATCH ()-[r:T]->() WHERE r.id = 10 SET r = {id: 10}",
		"MATCH ()-[r:T]->() WHERE r.id = 11 SET r += {w: 3}",
		"MATCH ()-[r:T]->() WHERE r.id = 12 SET r = {id: 12, w: 0}",
		"MATCH ()-[r:T]->() WHERE r.id = 13 SET r.w = r.w", // unchanged value
	} {
		mustQuery(t, db, q, nil)
		check("after " + q)
	}
}
