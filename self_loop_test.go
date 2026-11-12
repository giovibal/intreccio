package intreccio

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// selfLoopDB:
//
//	(a)-[:T {k: 1}]->(a)    relationship "loop"
//	(a)-[:T {k: 2}]->(b)    relationship "ab"
//	(a)-[:U {k: 2}]->(b)    relationship "ab2", parallel to ab
func selfLoopDB(t *testing.T) *DB {
	t.Helper()
	db := newDB(t)
	mustQuery(t, db, "CREATE (a:X {n: 'a'}), (b:X {n: 'b'})", nil)
	mustQuery(t, db, "MATCH (a:X {n: 'a'}) CREATE (a)-[:T {name: 'loop', k: 1}]->(a)", nil)
	mustQuery(t, db, "MATCH (a:X {n: 'a'}), (b:X {n: 'b'}) CREATE (a)-[:T {name: 'ab', k: 2}]->(b), (a)-[:U {name: 'ab2', k: 2}]->(b)", nil)
	return db
}

// rowSet renders the rows ("a 1, b 2") and sorts them: the tests below check
// which rows come back, and how many times, not their order.
func rowSet(res *Result) string {
	parts := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		cells := make([]string, len(r))
		for j, c := range r {
			cells[j] = fmt.Sprint(c)
		}
		parts[i] = strings.Join(cells, " ")
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// TestUndirectedSelfLoop covers #76: an undirected pattern matched a
// self-loop twice, once from the outgoing adjacency and once from the
// incoming one. It is one relationship, so it matches once.
func TestUndirectedSelfLoop(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"undirected: the loop once, an ordinary relationship once per orientation",
			"MATCH (a:X)-[r:T]-(b) RETURN a.n, r.name, b.n",
			"a ab b, a loop a, b ab a"},
		{"undirected, any type: parallel relationships keep one row each per orientation",
			"MATCH (a:X)-[r]-(b) RETURN a.n, r.name, b.n",
			"a ab b, a ab2 b, a loop a, b ab a, b ab2 a"},
		{"undirected, both ends the same variable",
			"MATCH (a:X)-[r]-(a) RETURN a.n, r.name",
			"a loop"},
		{"undirected, end bound by an earlier pattern",
			"MATCH (a:X {n: 'a'}), (b:X {n: 'a'}) MATCH (a)-[r:T]-(b) RETURN r.name",
			"loop"},
		{"undirected count",
			"MATCH (a:X {n: 'a'})-[r:T]-() RETURN count(r)",
			"2"},
		{"undirected, relationship not returned",
			"MATCH (a:X {n: 'a'})-[:T]-(b) RETURN b.n",
			"a, b"},
		{"undirected, nothing but the count used",
			"MATCH (:X {n: 'a'})-[:T]-() RETURN count(*)",
			"2"},
		{"directed out is unchanged",
			"MATCH (a:X)-[r:T]->(b) RETURN a.n, r.name, b.n",
			"a ab b, a loop a"},
		{"directed in is unchanged",
			"MATCH (a:X)<-[r:T]-(b) RETURN a.n, r.name, b.n",
			"a loop a, b ab a"},
		{"variable length, one hop",
			"MATCH (a:X {n: 'a'})-[r:T*1..1]-(b) RETURN b.n, size(r)",
			"a 1, b 1"},
		{"variable length: the loop is used once per path",
			"MATCH (a:X {n: 'a'})-[r:T*1..2]-(b) RETURN b.n, size(r)",
			"a 1, b 1, b 2"},
		{"variable length, both ends the same node",
			"MATCH (a:X {n: 'a'})-[r:T*1..1]-(a) RETURN size(r)",
			"1"},
	}
	db := selfLoopDB(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := db.Query(context.Background(), tc.query, nil)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := rowSet(res); got != tc.want {
				t.Errorf("%s\n got: %s\nwant: %s", tc.query, got, tc.want)
			}
		})
	}

	// The same patterns through a relationship property index, which finds
	// the relationship first and then binds its endpoints.
	mustQuery(t, db, "CREATE INDEX FOR ()-[r:T]-() ON (r.k)", nil)
	indexed := []struct {
		name, query, want string
	}{
		{"index seek: the loop once",
			"MATCH (a)-[r:T {k: 1}]-(b) RETURN a.n, r.name, b.n",
			"a loop a"},
		{"index seek: an ordinary relationship once per orientation",
			"MATCH (a)-[r:T {k: 2}]-(b) RETURN a.n, r.name, b.n",
			"a ab b, b ab a"},
		{"index seek, both ends the same variable",
			"MATCH (a)-[r:T {k: 1}]-(a) RETURN a.n, r.name",
			"a loop"},
	}
	for _, tc := range indexed {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := db.Explain(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if !strings.Contains(plan, "RelByProperty") {
				t.Fatalf("expected a relationship index seek, got:\n%s", plan)
			}
			if got := rowSet(mustQuery(t, db, tc.query, nil)); got != tc.want {
				t.Errorf("%s\n got: %s\nwant: %s", tc.query, got, tc.want)
			}
		})
	}
}
