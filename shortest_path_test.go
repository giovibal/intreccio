package intreccio

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// shortestGraph: a -KNOWS-> b -KNOWS-> c -KNOWS-> d, a diamond a -> x/y -> e,
// and a LIKES shortcut a -> d.
func shortestGraph(t *testing.T) *DB {
	db := newDB(t)
	mustQuery(t, db, `
		CREATE (a:P {name: 'a'}), (b:P {name: 'b'}), (c:P {name: 'c'}), (d:P {name: 'd'}),
		       (x:P {name: 'x'}), (y:P {name: 'y'}), (e:P {name: 'e'}), (:P {name: 'lonely'}),
		       (a)-[:KNOWS]->(b), (b)-[:KNOWS]->(c), (c)-[:KNOWS]->(d), (a)-[:LIKES]->(d),
		       (a)-[:KNOWS]->(x), (a)-[:KNOWS]->(y), (x)-[:KNOWS]->(e), (y)-[:KNOWS]->(e)`, nil)
	return db
}

func rowsOf(t *testing.T, db *DB, cypher string) string {
	t.Helper()
	return fmt.Sprint(mustQuery(t, db, cypher, nil).Rows)
}

const ends = "MATCH (s:P {name: $s}), (t:P {name: $t}) "

func TestShortestPath(t *testing.T) {
	db := shortestGraph(t)
	q := func(s, tt, rest string) string {
		t.Helper()
		res := mustQuery(t, db, ends+rest, map[string]any{"s": s, "t": tt})
		return fmt.Sprint(res.Rows)
	}
	names := "RETURN [n IN nodes(p) | n.name] AS names, [r IN relationships(p) | type(r)] AS types, length(p) AS len"

	tests := []struct{ s, t, match, want string }{
		{"a", "d", "MATCH p = shortestPath((s)-[*]->(t)) " + names, "[[[a d] [LIKES] 1]]"},
		{"a", "d", "MATCH p = shortestPath((s)-[:KNOWS*]->(t)) " + names, "[[[a b c d] [KNOWS KNOWS KNOWS] 3]]"},
		{"a", "d", "MATCH p = shortestPath((s)-[:KNOWS*..2]->(t)) " + names, "[]"},
		{"a", "d", "MATCH p = shortestPath((s)-[:KNOWS|LIKES*]->(t)) " + names, "[[[a d] [LIKES] 1]]"},
		{"d", "a", "MATCH p = shortestPath((s)-[*]->(t)) " + names, "[]"},
		{"d", "a", "MATCH p = shortestPath((s)<-[:KNOWS*]-(t)) " + names, "[[[d c b a] [KNOWS KNOWS KNOWS] 3]]"},
		{"d", "a", "MATCH p = shortestPath((s)-[:KNOWS*]-(t)) " + names, "[[[d c b a] [KNOWS KNOWS KNOWS] 3]]"},
		{"a", "lonely", "MATCH p = shortestPath((s)-[*]-(t)) " + names, "[]"},
		{"a", "d", "MATCH p = shortestPath((s)-[:NOPE*]-(t)) " + names, "[]"},
		{"a", "a", "MATCH p = shortestPath((s)-[*0..]-(t)) " + names, "[[[a] [] 0]]"},
		{"a", "a", "MATCH p = shortestPath((s)-[*]-(t)) " + names, "[]"},
		{"a", "e", "MATCH p = allShortestPaths((s)-[*]->(t)) " + names + " ORDER BY names", "[[[a x e] [KNOWS KNOWS] 2] [[a y e] [KNOWS KNOWS] 2]]"},
		{"a", "e", "MATCH p = shortestPath((s)-[*]->(t)) RETURN length(p)", "[[2]]"},
		{"a", "e", "MATCH shortestPath((s)-[r*]->(t)) RETURN size(r)", "[[2]]"},
		{"a", "d", "MATCH p = shortestPath((s)-[*]->(t)) RETURN p = p", "[[true]]"},
		{"a", "e", "MATCH p = allShortestPaths((s)-[*]->(t)) RETURN count(DISTINCT p)", "[[2]]"},
	}
	for _, tc := range tests {
		if got := q(tc.s, tc.t, tc.match); got != tc.want {
			t.Errorf("%s->%s %s\n got %s\nwant %s", tc.s, tc.t, tc.match, got, tc.want)
		}
	}
}

func TestShortestPathBinding(t *testing.T) {
	db := shortestGraph(t)
	want := "[[3]]"
	for _, q := range []string{
		// endpoints bound by an earlier clause
		"MATCH (s:P {name: 'a'}) MATCH (t:P {name: 'd'}) MATCH p = shortestPath((s)-[:KNOWS*]->(t)) RETURN length(p)",
		// endpoints bound later in the same MATCH
		"MATCH p = shortestPath((s)-[:KNOWS*]->(t)), (s:P {name: 'a'}), (t:P {name: 'd'}) RETURN length(p)",
		// WHERE on the endpoints is fine
		"MATCH (s:P), (t:P), p = shortestPath((s)-[:KNOWS*]->(t)) WHERE s.name = 'a' AND t.name = 'd' RETURN length(p)",
		// filtering on the path in a following WITH
		"MATCH (s:P {name: 'a'}), (t:P {name: 'd'}), p = shortestPath((s)-[:KNOWS*]->(t)) WITH p WHERE length(p) > 1 RETURN length(p)",
	} {
		if got := rowsOf(t, db, q); got != want {
			t.Errorf("%s\n got %s, want %s", q, got, want)
		}
	}
	// OPTIONAL MATCH yields null when there is no path.
	got := rowsOf(t, db, "MATCH (s:P {name: 'a'}), (t:P {name: 'lonely'}) OPTIONAL MATCH p = shortestPath((s)-[*]-(t)) RETURN t.name, p IS NULL")
	if got != "[[lonely true]]" {
		t.Errorf("optional no-path = %s", got)
	}
	// One search per pair of endpoints.
	got = rowsOf(t, db, "MATCH (s:P {name: 'a'}), (t:P) WHERE t.name IN ['b', 'c', 'd'] MATCH p = shortestPath((s)-[:KNOWS*]->(t)) RETURN t.name, length(p) ORDER BY t.name")
	if got != "[[b 1] [c 2] [d 3]]" {
		t.Errorf("per-row search = %s", got)
	}
	plan, err := db.Explain(context.Background(), "MATCH (s:P {name: 'a'}), (t:P {name: 'd'}), p = allShortestPaths((s)-[:KNOWS*..5]-(t)) RETURN p")
	if err != nil || !strings.Contains(plan, "AllShortestPaths(s)-[:KNOWS*..5]-(t) AS p") {
		t.Errorf("explain = %v\n%s", err, plan)
	}
}

func TestShortestPathErrors(t *testing.T) {
	db := shortestGraph(t)
	for _, q := range []string{
		"MATCH p = shortestPath((s)-[*]-(t)) RETURN p",                                              // unbound endpoints
		"MATCH (s:P), (t:P), p = shortestPath((s)-[*]-(t)) WHERE length(p) > 1 RETURN p",            // WHERE on the path
		"MATCH (s:P), (t:P), p = shortestPath((s)-[r*]-(t)) WHERE size(r) > 1 RETURN p",             // WHERE on its relationships
		"MATCH (s:P), (t:P), p = shortestPath((s)-[]-(t)) RETURN p",                                 // not variable-length
		"MATCH (s:P), (t:P), p = shortestPath((s)-[*2..]-(t)) RETURN p",                             // min > 1
		"MATCH (s:P), (t:P), p = shortestPath((s:P)-[*]-(t)) RETURN p",                              // labels on an endpoint
		"MATCH (s:P), (t:P), p = shortestPath((s)-[*]-(m)-[*]-(t)) RETURN p",                        // two relationships
		"MATCH (s:P), (t:P), p = shortestPath((s)-[* {w: 1}]-(t)) RETURN p",                         // relationship properties
		"MATCH (s:P {name: 'a'}), (t:P {name: 'b'}) CREATE p = shortestPath((s)-[:R*]->(t))",        // in CREATE
		"MATCH (s:P {name: 'a'}), (t:P {name: 'b'}) MERGE p = shortestPath((s)-[:R*]->(t))",         // in MERGE
		"MATCH (s:P {name: 'a'}), (t:P {name: 'd'}), p = shortestPath((s)-[*]->(t)) RETURN size(p)", // size of a path
	} {
		if _, err := db.Query(context.Background(), q, nil); err == nil {
			t.Errorf("%s: succeeded, want an error", q)
		}
	}
}
