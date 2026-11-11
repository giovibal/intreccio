package intreccio

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// relGraph seeds the same random graph into db: nodes with labels L1/L2 and
// :T / :U relationships whose w and s properties mix ints, floats, strings,
// lists and missing values, plus self-loops.
func relGraph(t *testing.T, db *DB, seed uint64) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 7))
	const n = 30
	for i := range n {
		label := []string{"L1", "L2"}[i%2]
		mustQuery(t, db, fmt.Sprintf("CREATE (:%s {id: %d})", label, i), nil)
	}
	ws := []string{"1", "1.0", "2", "3", "'x'", "[1, 2]", "1.5"}
	for i := range 120 {
		a, b := r.IntN(n), r.IntN(n)
		if i%15 == 0 {
			b = a // self-loop
		}
		typ := "T"
		if r.IntN(4) == 0 {
			typ = "U"
		}
		props := fmt.Sprintf("id: %d", i)
		if r.IntN(5) > 0 {
			props += ", w: " + ws[r.IntN(len(ws))]
		}
		if r.IntN(2) == 0 {
			props += fmt.Sprintf(", s: '%c'", 'x'+rune(r.IntN(3)))
		}
		mustQuery(t, db, fmt.Sprintf("MATCH (a {id: %d}), (b {id: %d}) CREATE (a)-[:%s {%s}]->(b)", a, b, typ, props), nil)
	}
}

func sortedRows(res *Result) []string {
	out := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = fmt.Sprint(r)
	}
	sort.Strings(out)
	return out
}

// TestRelIndexEquivalence covers #37: every query returns the same rows with
// and without relationship indexes, and the indexed plans do seek.
func TestRelIndexEquivalence(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) { relIndexEquivalence(t, seed) })
	}
}

func relIndexEquivalence(t *testing.T, seed uint64) {
	ctx := context.Background()
	plain, indexed := newDB(t), newDB(t)
	relGraph(t, plain, seed)
	mustQuery(t, indexed, "CREATE INDEX FOR ()-[r:T]-() ON (r.w)", nil) // before the data
	relGraph(t, indexed, seed)
	mustQuery(t, indexed, "CREATE INDEX t_s FOR ()-[r:T]->() ON (r.s)", nil) // backfilled

	queries := []struct {
		q    string
		seek bool // the indexed plan must use RelByProperty
	}{
		{"MATCH (a)-[r:T {w: 1}]->(b) RETURN a.id, r.id, b.id", true},
		{"MATCH (a)<-[r:T {w: 1}]-(b) RETURN a.id, r.id, b.id", true},
		{"MATCH (a)-[r:T {w: 1}]-(b) RETURN a.id, r.id, b.id", true},
		{"MATCH (a)-[r:T]->(b) WHERE r.w = 1.0 RETURN a.id, r.id, b.id", true},
		{"MATCH (a)-[r:T]->(b) WHERE r.w = 2 AND r.s = 'y' RETURN r.id", true},
		{"MATCH (a:L1)-[r:T {s: 'x'}]->(b:L2) RETURN a.id, r.id, b.id", true},
		{"MATCH (a)-[r:T {w: 2}]->(b)-[:U]->(c) RETURN a.id, b.id, c.id", true},
		{"MATCH (c)-[:U]->(a)-[r:T {w: 2}]->(b) RETURN a.id, b.id, c.id", true},
		{"MATCH (a)-[r:T {w: 3}]->(a) RETURN a.id, r.id", true},
		{"MATCH (a)-[r:T {w: 3}]-(a) RETURN a.id, r.id", true},
		{"MATCH (a)-[r:T {w: [1, 2]}]->(b) RETURN r.id", true},
		{"MATCH (a)-[r:T {w: 'x'}]->(b) RETURN r.id", true},
		{"MATCH (a)-[r:T {w: 99}]->(b) RETURN r.id", true},
		{"MATCH (a)-[r:T {w: null}]->(b) RETURN r.id", true}, // seeks nothing
		{"UNWIND [1, 2, 'x', 1.5] AS v MATCH (a)-[r:T {w: v}]->(b) RETURN v, r.id", true},
		{"MATCH (a {id: 3})-[r:T {w: 1}]->(b) RETURN r.id", true},            // no node index: the relationship is the anchor
		{"MATCH (a {id: 3}) MATCH (a)-[r:T {w: 1}]->(b) RETURN r.id", false}, // node bound first
		{"MATCH (a:L1) WHERE a.id < 10 OPTIONAL MATCH (a)-[r:T {w: 2}]->(b) RETURN a.id, r.id", false},
		{"OPTIONAL MATCH ()-[r:T {w: 99}]->() RETURN r", true},
		{"MATCH ()-[r:U {w: 1}]->() RETURN r.id", false}, // no index on :U
		{"MATCH (a)-[r:T|U {w: 1}]->(b) RETURN r.id", false},
		{"MATCH p = shortestPath((a {id: 1})-[:T*]-(b {id: 2})) RETURN length(p)", false},
	}
	check := func(stage string) {
		t.Helper()
		for _, tc := range queries {
			want, err1 := plain.Query(ctx, tc.q, nil)
			got, err2 := indexed.Query(ctx, tc.q, nil)
			if (err1 == nil) != (err2 == nil) {
				t.Errorf("%s: %s: errors differ: %v vs %v", stage, tc.q, err1, err2)
				continue
			}
			if err1 != nil {
				continue
			}
			if !reflect.DeepEqual(sortedRows(want), sortedRows(got)) {
				t.Errorf("%s: %s\n without index %v\n    with index %v", stage, tc.q, sortedRows(want), sortedRows(got))
			}
			plan, err := indexed.Explain(ctx, tc.q)
			if err != nil {
				t.Errorf("explain %s: %v", tc.q, err)
				continue
			}
			if uses := strings.Contains(plan, "RelByProperty"); uses != tc.seek {
				t.Errorf("%s: %s: uses RelByProperty = %v, want %v\n%s", stage, tc.q, uses, tc.seek, plan)
			}
		}
	}
	check("initial")

	// Writes keep the index in sync.
	for _, w := range []string{
		"MATCH ()-[r:T {w: 1}]->() WHERE r.id < 40 DELETE r", // deterministic in both plans
		"MATCH (a {id: 5}), (b {id: 6}) CREATE (a)-[:T {id: 500, w: 1, s: 'y'}]->(b), (b)-[:T {id: 501, w: 2.0}]->(b)",
		"MATCH (a {id: 7}) DETACH DELETE a",
	} {
		mustQuery(t, plain, w, nil)
		mustQuery(t, indexed, w, nil)
	}
	check("after writes")
}

func TestRelIndexCommands(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	mustQuery(t, db, "CREATE (:P {id: 1})-[:KNOWS {since: 2020}]->(:P {id: 2})", nil)
	mustQuery(t, db, "CREATE INDEX FOR (p:P) ON (p.id)", nil)
	mustQuery(t, db, "CREATE INDEX knows_since FOR ()-[k:KNOWS]-() ON (k.since)", nil)
	mustQuery(t, db, "CREATE INDEX FOR ()-[k:KNOWS]-() ON (k.since)", nil) // idempotent
	got := rowsOf(t, db, "SHOW INDEXES")
	if got != "[[index_P_id P id NODE] [knows_since KNOWS since RELATIONSHIP]]" {
		t.Errorf("SHOW INDEXES = %s", got)
	}
	for _, q := range []string{
		"CREATE INDEX other FOR ()-[k:KNOWS]-() ON (k.since)",  // same index, other name
		"CREATE INDEX knows_since FOR (p:P) ON (p.name)",       // name taken by a relationship index
		"CREATE INDEX index_P_id FOR ()-[k:KNOWS]-() ON (k.x)", // name taken by a node index
		"CREATE INDEX FOR ()-[:KNOWS]-() ON (k.since)",         // no variable
		"CREATE INDEX FOR ()-[k:KNOWS|LIKES]-() ON (k.since)",  // two types
		"CREATE INDEX FOR (a)-[k:KNOWS]-() ON (k.since)",       // named endpoint
	} {
		if _, err := db.Query(ctx, q, nil); err == nil {
			t.Errorf("%s: succeeded, want an error", q)
		}
	}
	const q = "MATCH (a)-[k:KNOWS {since: 2020}]->(b) RETURN a.id, b.id"
	plan, _ := db.Explain(ctx, q)
	if !strings.Contains(plan, "RelByProperty((a)-[k:KNOWS]->(b) {since = 2020})") {
		t.Errorf("plan does not seek the relationship:\n%s", plan)
	}
	if got := rowsOf(t, db, q); got != "[[1 2]]" {
		t.Errorf("%s = %s", q, got)
	}
	// A node index seek stays the preferred anchor.
	plan, _ = db.Explain(ctx, "MATCH (a:P {id: 1})-[k:KNOWS {since: 2020}]->(b) RETURN b.id")
	if !strings.Contains(plan, "NodeByProperty") || strings.Contains(plan, "RelByProperty") {
		t.Errorf("node seek not preferred:\n%s", plan)
	}
	mustQuery(t, db, "DROP INDEX knows_since", nil)
	plan, _ = db.Explain(ctx, q)
	if strings.Contains(plan, "RelByProperty") {
		t.Errorf("dropped index still used:\n%s", plan)
	}
	if got := rowsOf(t, db, q); got != "[[1 2]]" {
		t.Errorf("after drop %s = %s", q, got)
	}
	if got := rowsOf(t, db, "SHOW INDEXES"); got != "[[index_P_id P id NODE]]" {
		t.Errorf("SHOW INDEXES after drop = %s", got)
	}
}
