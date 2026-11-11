package intreccio

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/sema"
)

func TestNewAnchorEligibility(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"MATCH (n:Person) RETURN n.name", true},
		{"MATCH (n) RETURN n", true},
		{"MATCH (a:Person)-[r:KNOWS]->(b) WHERE a.age > b.age RETURN a.name, type(r), b", true},
		{"MATCH (a)-[:K]->()<-[:K]-(c) RETURN a, c", true},
		{"MATCH (a:A), (b:B) RETURN a.x, b.y", true},
		{"MATCH (a)-[:K]-(b) RETURN *", true},
		{"MATCH (a {k: $p}) RETURN toUpper(a.name) AS n, [a.x, 1] AS l", true},

		{"MATCH (n:Person) RETURN count(n)", false},                   // aggregation
		{"MATCH (n:Person) RETURN n.city, collect(n.name)", false},    // aggregation
		{"MATCH (n:Person) RETURN size(collect(n.name)) AS c", false}, // nested aggregation
		{"MATCH (n:Person) RETURN DISTINCT n.city", false},            // DISTINCT
		{"MATCH (n:Person) RETURN n.name ORDER BY n.name", false},     // ORDER BY
		{"MATCH (n:Person) RETURN n.name LIMIT 3", false},             // LIMIT
		{"MATCH (n:Person) RETURN n.name SKIP 1", false},              // SKIP
		{"MATCH (a)-[:K*1..3]->(b) RETURN a, b", false},               // variable length
		{"MATCH (a) OPTIONAL MATCH (a)-[:K]->(b) RETURN a, b", false}, // OPTIONAL MATCH
		{"MATCH (a) MATCH (b) RETURN a, b", false},                    // two MATCH clauses
		{"MATCH (a) WITH a.x AS x RETURN x", false},                   // WITH
		{"MATCH (a) RETURN a.x AS x UNION MATCH (b) RETURN b.x AS x", false},
		{"UNWIND [1, 2] AS x RETURN x", false},
		{"RETURN 1 AS one", false},
	}
	for _, c := range cases {
		q, err := parser.Parse(c.src)
		if err != nil {
			t.Fatalf("parse(%q): %v", c.src, err)
		}
		res, err := sema.Analyze(q)
		if err != nil {
			t.Fatalf("sema(%q): %v", c.src, err)
		}
		a := newAnchor(q, res.Columns)
		if (a != nil) != c.want {
			t.Errorf("anchorable(%q) = %v, want %v", c.src, a != nil, c.want)
		}
		if a == nil {
			continue
		}
		// The rewrite must be a valid query producing the original columns
		// followed by one ID per pattern element.
		if len(a.columns) <= a.nCols || len(a.nodeVars) == 0 {
			t.Errorf("%q: anchor has %d columns for %d of its own, %d node positions", c.src, len(a.columns), a.nCols, len(a.nodeVars))
		}
		if q.Clauses[0] == a.query.Clauses[0] {
			t.Errorf("%q: the rewrite shares its MATCH clause with the original query", c.src)
		}
	}
}

// rowBag is a multiset of encoded rows.
type rowBag map[string]int

func (b rowBag) keys() []string {
	var out []string
	for k, n := range b {
		for range n {
			out = append(out, fmt.Sprintf("%x", k))
		}
	}
	sort.Strings(out)
	return out
}

func bagOf(t *testing.T, rows [][]any) rowBag {
	t.Helper()
	b := rowBag{}
	for _, row := range rows {
		enc, err := encodeRow(row)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		b[string(enc)]++
	}
	return b
}

// apply folds an evaluation into the bag, failing on a row that is removed
// without being there or on a decoded row that disagrees with its encoding.
func (b rowBag) apply(t *testing.T, ev *evaluation) {
	t.Helper()
	if len(ev.added) != len(ev.addedEnc) {
		t.Fatalf("evaluation has %d added rows and %d encodings", len(ev.added), len(ev.addedEnc))
	}
	for _, row := range ev.removed {
		if b[string(row)] == 0 {
			t.Fatalf("evaluation removed a row the result did not hold")
		}
		if b[string(row)]--; b[string(row)] == 0 {
			delete(b, string(row))
		}
	}
	for i, row := range ev.addedEnc {
		enc, err := encodeRow(ev.added[i])
		if err != nil || string(enc) != string(row) {
			t.Fatalf("added row %d: decoded and encoded forms disagree (%v)", i, err)
		}
		b[string(row)]++
	}
}

// graphFuzzer applies random mutations through Cypher.
type graphFuzzer struct {
	t  *testing.T
	db *DB
	r  *rand.Rand
}

func (f *graphFuzzer) ids(cypher string) []any {
	f.t.Helper()
	res, err := f.db.Query(context.Background(), cypher, nil)
	if err != nil {
		f.t.Fatalf("%s: %v", cypher, err)
	}
	out := make([]any, len(res.Rows))
	for i, row := range res.Rows {
		out[i] = row[0]
	}
	return out
}

func (f *graphFuzzer) exec(cypher string, params map[string]any) {
	f.t.Helper()
	if _, err := f.db.Query(context.Background(), cypher, params); err != nil {
		f.t.Fatalf("%s %v: %v", cypher, params, err)
	}
}

func (f *graphFuzzer) mutate() {
	r := f.r
	nodes := f.ids("MATCH (n) RETURN id(n)")
	pick := func() any { return nodes[r.Intn(len(nodes))] }
	label := func() string { return []string{"A", "B", "C"}[r.Intn(3)] }
	key := func() string { return []string{"k1", "k2"}[r.Intn(2)] }
	val := func() any { return int64(r.Intn(3)) }
	typ := func() string { return []string{"T1", "T2"}[r.Intn(2)] }

	op := r.Intn(12)
	if len(nodes) < 2 {
		op = 0
	}
	switch op {
	case 0, 1:
		f.exec(fmt.Sprintf("CREATE (:%s {k1: $a, k2: $b})", label()), map[string]any{"a": val(), "b": val()})
	case 2:
		f.exec("CREATE ({k1: $a})", map[string]any{"a": val()})
	case 3, 4:
		f.exec(fmt.Sprintf("MATCH (n) WHERE id(n) = $id SET n.%s = $v", key()), map[string]any{"id": pick(), "v": val()})
	case 5:
		f.exec(fmt.Sprintf("MATCH (n) WHERE id(n) = $id REMOVE n.%s", key()), map[string]any{"id": pick()})
	case 6:
		f.exec(fmt.Sprintf("MATCH (n) WHERE id(n) = $id SET n:%s", label()), map[string]any{"id": pick()})
	case 7:
		f.exec(fmt.Sprintf("MATCH (n) WHERE id(n) = $id REMOVE n:%s", label()), map[string]any{"id": pick()})
	case 8, 9:
		// Self-loops and parallel relationships are welcome.
		f.exec(fmt.Sprintf("MATCH (a), (b) WHERE id(a) = $a AND id(b) = $b CREATE (a)-[:%s]->(b)", typ()),
			map[string]any{"a": pick(), "b": pick()})
	case 10:
		if rels := f.ids("MATCH ()-[r]->() RETURN id(r)"); len(rels) > 0 {
			f.exec("MATCH ()-[r]->() WHERE id(r) = $id DELETE r", map[string]any{"id": rels[r.Intn(len(rels))]})
		}
	default:
		f.exec("MATCH (n) WHERE id(n) = $id DETACH DELETE n", map[string]any{"id": pick()})
	}
}

// TestAnchoredEvaluationMatchesTheQuery is the property test of anchored
// evaluation: over random mutation sequences, re-evaluating a live query around
// the nodes that changed must leave its result exactly equal to running the
// query from scratch — and must report exactly the difference.
func TestAnchoredEvaluationMatchesTheQuery(t *testing.T) {
	queries := []string{
		"MATCH (n:A) RETURN n.k1 AS k",
		"MATCH (n) RETURN n",
		"MATCH (n) WHERE n.k1 = 1 RETURN id(n) AS id, labels(n) AS ls",
		"MATCH (a:A)-[r:T1]->(b) RETURN a.k1 AS a, b.k2 AS b, type(r) AS t",
		"MATCH (a)-[:T1]->(b)-[:T2]->(c) RETURN id(a) AS a, b, c.k1 AS c",
		"MATCH (a)-[r]-(b) WHERE a.k1 = b.k1 RETURN a.k1 AS k, id(r) AS r",
		"MATCH (a)-[:T1]->(a) RETURN a.k1 AS k",
		"MATCH (a:A {k1: 1})-[r]->(b:B) WHERE b.k2 > 0 RETURN *",
		"MATCH (a)-[:T1]->(b)<-[:T1]-(c) RETURN a.k1 AS a, c.k1 AS c",
		"MATCH (a:A), (b:B) WHERE a.k1 = b.k1 RETURN a.k2 AS a, b.k2 AS b",
		"MATCH (a:C)-[:T2]->(b:C) RETURN toString(a.k1) AS a, [b.k1, b.k2] AS b",
	}
	for seed := int64(1); seed <= 3; seed++ {
		db := newDB(t)
		fz := &graphFuzzer{t: t, db: db, r: rand.New(rand.NewSource(seed))}
		for range 6 {
			fz.mutate()
		}

		type live struct {
			src string
			lq  *liveQuery
			bag rowBag
		}
		var lives []*live
		for _, src := range queries {
			q, err := parser.Parse(src)
			if err != nil {
				t.Fatalf("parse(%q): %v", src, err)
			}
			res, err := sema.Analyze(q)
			if err != nil {
				t.Fatalf("sema(%q): %v", src, err)
			}
			lq := db.newLiveQuery(context.Background(), q, res.Columns, nil, 0)
			if lq.anchor == nil {
				t.Fatalf("%q is not anchorable", src)
			}
			defer lq.close()
			l := &live{src: src, lq: lq, bag: rowBag{}}
			ev, err := lq.evaluate()
			if err != nil {
				t.Fatalf("initial evaluation of %q: %v", src, err)
			}
			l.bag.apply(t, ev)
			lives = append(lives, l)
		}
		fullBefore := db.liveStats.full.Load()

		for step := range 80 {
			// One or several commits per evaluation: coalescing is part of it.
			for range 1 + fz.r.Intn(3) {
				fz.mutate()
			}
			for _, l := range lives {
				ev, err := l.lq.evaluate()
				if err != nil {
					t.Fatalf("seed %d step %d: evaluate %q: %v", seed, step, l.src, err)
				}
				l.bag.apply(t, ev)
				res, err := db.Query(context.Background(), l.src, nil)
				if err != nil {
					t.Fatalf("query %q: %v", l.src, err)
				}
				want := bagOf(t, res.Rows)
				if got := l.bag.keys(); fmt.Sprint(got) != fmt.Sprint(want.keys()) {
					t.Fatalf("seed %d step %d: %q diverged:\n got %d rows\nwant %d rows", seed, step, l.src, len(got), len(want.keys()))
				}
				if n := l.lq.anchor.rows; n != int64(len(res.Rows)) {
					t.Fatalf("seed %d step %d: %q: the index counts %d rows for a result of %d", seed, step, l.src, n, len(res.Rows))
				}
			}
		}
		if got := db.liveStats.full.Load(); got != fullBefore {
			t.Errorf("seed %d: %d evaluations fell back to a full run", seed, got-fullBefore)
		}
		if db.liveStats.anchored.Load() == 0 {
			t.Errorf("seed %d: no evaluation was anchored", seed)
		}

		// With every node gone the index must be empty too.
		fz.exec("MATCH (n) DETACH DELETE n", nil)
		for _, l := range lives {
			ev, err := l.lq.evaluate()
			if err != nil {
				t.Fatal(err)
			}
			l.bag.apply(t, ev)
			if len(l.bag) != 0 || len(l.lq.anchor.matches) != 0 || len(l.lq.anchor.byNode) != 0 || l.lq.anchor.rows != 0 {
				t.Errorf("seed %d: %q: %d rows, %d matches, %d indexed nodes left after deleting everything",
					seed, l.src, len(l.bag), len(l.lq.anchor.matches), len(l.lq.anchor.byNode))
			}
		}
	}
}

// TestAnchoredFallsBackToAFullRun checks the cases in which the changes cannot
// be listed: the query is then run in full, and the result is still right.
func TestAnchoredFallsBackToAFullRun(t *testing.T) {
	db := newDB(t)
	const src = "MATCH (n:N) RETURN n.i AS i"
	q, _ := parser.Parse(src)
	res, _ := sema.Analyze(q)
	lq := db.newLiveQuery(context.Background(), q, res.Columns, nil, 0)
	defer lq.close()
	bag := rowBag{}
	check := func(what string, wantFull bool) {
		t.Helper()
		full, anchored := db.liveStats.full.Load(), db.liveStats.anchored.Load()
		ev, err := lq.evaluate()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		bag.apply(t, ev)
		got, err := db.Query(context.Background(), src, nil)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(bag.keys()) != fmt.Sprint(bagOf(t, got.Rows).keys()) {
			t.Errorf("%s: result diverged", what)
		}
		ranFull := db.liveStats.full.Load() > full
		if ranFull != wantFull || (!wantFull && db.liveStats.anchored.Load() == anchored) {
			t.Errorf("%s: full run = %v, want %v", what, ranFull, wantFull)
		}
	}

	check("first evaluation", true)
	mustExec(t, db, "CREATE (:N {i: 1})")
	check("one node changed", false)

	db.hub.WakeAll()
	mustExec(t, db, "CREATE (:N {i: 2})")
	check("unknown change", true)
	check("nothing changed", false)

	// More changed nodes than the watcher is willing to list.
	mustExec(t, db, "UNWIND $is AS i CREATE (:N {i: i})", map[string]any{"is": manyInts(5000)})
	check("too many changed nodes", true)
	mustExec(t, db, "MATCH (n:N {i: 7}) SET n.i = -7")
	check("back to anchored", false)
}

func manyInts(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = int64(100 + i)
	}
	return out
}

// lateStore commits like the store it wraps, then holds the commit back from
// being reported until released: the window in which a write is visible to
// readers but its effect has not been announced.
type lateStore struct {
	storage.Store
	committed chan struct{}
	release   chan struct{}
}

func (s *lateStore) Update(fn func(storage.Txn) error) error {
	err := s.Store.Update(fn)
	close(s.committed)
	<-s.release
	return err
}

// TestAnchoredEvaluationSeesAConsistentSnapshot reproduces the race anchored
// evaluation must not lose: a write that has committed, and so is visible to
// the evaluation's read transaction, but has not been reported yet. Evaluating
// around the reported nodes alone would mix rows of two different states.
func TestAnchoredEvaluationSeesAConsistentSnapshot(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Pair {name: 'x', v: 1}), (:Pair {name: 'y', v: 1})")
	var x, y uint64
	{
		res, err := db.Query(context.Background(), "MATCH (p:Pair) RETURN p.name AS n, id(p) AS id ORDER BY n", nil)
		if err != nil {
			t.Fatal(err)
		}
		x, y = uint64(res.Rows[0][1].(int64)), uint64(res.Rows[1][1].(int64))
	}

	const src = "MATCH (p:Pair) RETURN p.name AS name, p.v AS v"
	q, _ := parser.Parse(src)
	res, _ := sema.Analyze(q)
	lq := db.newLiveQuery(context.Background(), q, res.Columns, nil, 0)
	defer lq.close()
	bag := rowBag{}
	ev, err := lq.evaluate()
	if err != nil {
		t.Fatal(err)
	}
	bag.apply(t, ev)

	// State (2, 1): reported, x is dirty.
	mustExec(t, db, "MATCH (p:Pair {name: 'x'}) SET p.v = 2")

	// State (3, 3): committed, and held back before it is reported.
	store := db.be.(*localBackend).store
	late := &lateStore{Store: store, committed: make(chan struct{}), release: make(chan struct{})}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- db.hub.Update(late, func(txn storage.Txn) error {
			if err := graph.SetProperty(txn, x, "v", 3); err != nil {
				return err
			}
			return graph.SetProperty(txn, y, "v", 3)
		})
	}()
	<-late.committed

	// The evaluation now reads (3, 3) but has only been told about x. It must
	// wait for the report rather than deliver the never-existing (3, 1).
	evalDone := make(chan *evaluation, 1)
	go func() {
		ev, err := lq.evaluate()
		if err != nil {
			t.Errorf("evaluate: %v", err)
		}
		evalDone <- ev
	}()
	select {
	case <-evalDone:
		t.Fatal("the evaluation did not wait for a committed write to be reported")
	case <-time.After(100 * time.Millisecond):
	}
	close(late.release)
	if err := <-writeDone; err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case ev := <-evalDone:
		bag.apply(t, ev)
	case <-time.After(subTimeout):
		t.Fatal("the evaluation never completed")
	}
	want := bagOf(t, [][]any{{"x", int64(3)}, {"y", int64(3)}})
	if fmt.Sprint(bag.keys()) != fmt.Sprint(want.keys()) {
		t.Fatalf("the evaluation delivered a state that never existed: %d rows differ from (x=3, y=3)", len(bag))
	}

	// What it re-marked costs one more round, which changes nothing.
	ev, err = lq.evaluate()
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.added) != 0 || len(ev.removed) != 0 {
		t.Errorf("the follow-up round reported a change: +%d -%d", len(ev.added), len(ev.removed))
	}
}

// TestAnchoredRowCap checks that the row cap holds when the result grows
// through anchored evaluations.
func TestAnchoredRowCap(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:N {i: 1}), (:N {i: 2})")
	q, _ := parser.Parse("MATCH (n:N) RETURN n.i AS i")
	res, _ := sema.Analyze(q)
	lq := db.newLiveQuery(context.Background(), q, res.Columns, nil, 3)
	defer lq.close()
	if _, err := lq.evaluate(); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	mustExec(t, db, "CREATE (:N {i: 3})")
	if _, err := lq.evaluate(); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	mustExec(t, db, "CREATE (:N {i: 4})")
	if _, err := lq.evaluate(); !errors.Is(err, ErrResultLimit) {
		t.Fatalf("over the cap: err = %v, want ErrResultLimit", err)
	}
}
