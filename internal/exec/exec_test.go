package exec

import (
	"sort"
	"sync"
	"testing"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/plan"
	"github.com/giovibal/intreccio/query/sema"
	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
)

// testCatalog adapts the catalog to plan.Catalog within a transaction.
type testCatalog struct{ txn storage.Txn }

func (c testCatalog) HasIndex(label, key string) bool {
	lid, ok, _ := catalog.LookupLabel(c.txn, label)
	if !ok {
		return false
	}
	kid, ok, _ := catalog.LookupKey(c.txn, key)
	if !ok {
		return false
	}
	has, _ := catalog.HasIndex(c.txn, lid, kid)
	return has
}

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func runQuery(t *testing.T, s storage.Store, cypher string, params map[string]any) [][]any {
	t.Helper()
	q, err := parser.Parse(cypher)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		t.Fatalf("sema: %v", err)
	}
	// We always run inside an Update transaction so the helper handles both
	// read and write queries; the overhead is negligible for tests.
	var rows [][]any
	err = s.Update(func(txn storage.Txn) error {
		p, err := plan.Plan(q, testCatalog{txn: txn})
		if err != nil {
			return err
		}
		rows, err = Run(p, semaRes.Columns, &Context{Txn: txn, Params: params})
		return err
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rows
}

// firstColumnStrings extracts column 0 of each row as sorted strings.
func firstColumnStrings(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) == 0 || r[0] == nil {
			out = append(out, "")
			continue
		}
		out = append(out, r[0].(string))
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

// seedSocial builds a small graph: Alice knows Bob and Carol; Bob knows Carol.
func seedSocial(t *testing.T, s storage.Store) {
	t.Helper()
	err := s.Update(func(tx storage.Txn) error {
		alice, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Alice", "email": "a@b.com"})
		if err != nil {
			return err
		}
		bob, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Bob", "email": "b@b.com"})
		if err != nil {
			return err
		}
		carol, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Carol", "email": "c@b.com"})
		if err != nil {
			return err
		}
		for _, e := range [][2]uint64{{alice, bob}, {alice, carol}, {bob, carol}} {
			if _, err := graph.CreateEdge(tx, "KNOWS", e[0], e[1], nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// The Phase 6 vertical slice: a read query with an Expand returns correct results
// from disk.
func TestExpandQuery(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s)

	rows := runQuery(t, s,
		"MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email = $e RETURN f.name",
		map[string]any{"e": "a@b.com"})

	got := firstColumnStrings(rows)
	want := []string{"Bob", "Carol"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandQueryUsesIndex(t *testing.T) {
	s := newStore(t)
	// Create an index on (Person, email) before seeding so `p` entries exist.
	if err := s.Update(func(tx storage.Txn) error {
		pid, err := catalog.InternLabel(tx, "Person")
		if err != nil {
			return err
		}
		eid, err := catalog.InternKey(tx, "email")
		if err != nil {
			return err
		}
		return catalog.AddIndex(tx, pid, eid)
	}); err != nil {
		t.Fatal(err)
	}
	seedSocial(t, s)

	rows := runQuery(t, s,
		"MATCH (p:Person)-[:KNOWS]->(f) WHERE p.email = $e RETURN f.name",
		map[string]any{"e": "a@b.com"})
	if got, want := firstColumnStrings(rows), []string{"Bob", "Carol"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLabelScanProjection(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s)

	rows := runQuery(t, s, "MATCH (p:Person) RETURN p.name AS name ORDER BY name", nil)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	want := []string{"Alice", "Bob", "Carol"} // ORDER BY name
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWhereComparison(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Young", "age": int64(20)}); err != nil {
			return err
		}
		if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Old", "age": int64(80)}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s, "MATCH (p:Person) WHERE p.age > 30 RETURN p.name", nil)
	if got, want := firstColumnStrings(rows), []string{"Old"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReturnLiteral(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "RETURN 1 + 2 AS three", nil)
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Errorf("got %v, want [[3]]", rows)
	}
}

func TestNoResultsForMissingLabel(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "MATCH (p:Ghost) RETURN p.name", nil)
	if len(rows) != 0 {
		t.Errorf("got %v, want no rows", rows)
	}
}

func TestCountStarEmptyAndPopulated(t *testing.T) {
	s := newStore(t)

	rows := runQuery(t, s, "MATCH (n) RETURN count(*) AS c", nil)
	if len(rows) != 1 || rows[0][0] != int64(0) {
		t.Errorf("empty db: got %v, want [[0]]", rows)
	}

	seedSocial(t, s) // 3 persons + 3 KNOWS edges
	rows = runQuery(t, s, "MATCH (n:Person) RETURN count(*) AS c", nil)
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Errorf("populated: got %v, want [[3]]", rows)
	}
}

func TestAggregateGroupBy(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, p := range []map[string]any{
			{"name": "A", "country": "IT"},
			{"name": "B", "country": "IT"},
			{"name": "C", "country": "DE"},
		} {
			if _, err := graph.CreateNode(tx, []string{"Person"}, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (p:Person) RETURN p.country AS country, count(*) AS n ORDER BY country", nil)
	if len(rows) != 2 {
		t.Fatalf("expected 2 groups, got %d (%v)", len(rows), rows)
	}
	if rows[0][0] != "DE" || rows[0][1] != int64(1) {
		t.Errorf("row[0] = %v, want [DE 1]", rows[0])
	}
	if rows[1][0] != "IT" || rows[1][1] != int64(2) {
		t.Errorf("row[1] = %v, want [IT 2]", rows[1])
	}
}

func TestAggregateSumAvgMinMax(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, age := range []int64{10, 20, 30, 40} {
			if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"age": age}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (p:Person) RETURN sum(p.age) AS s, avg(p.age) AS a, min(p.age) AS lo, max(p.age) AS hi", nil)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0][0] != int64(100) {
		t.Errorf("sum = %v, want 100", rows[0][0])
	}
	if rows[0][1] != float64(25) {
		t.Errorf("avg = %v, want 25", rows[0][1])
	}
	if rows[0][2] != int64(10) {
		t.Errorf("min = %v, want 10", rows[0][2])
	}
	if rows[0][3] != int64(40) {
		t.Errorf("max = %v, want 40", rows[0][3])
	}
}

func TestAggregateCollect(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s)

	rows := runQuery(t, s,
		"MATCH (p:Person) RETURN collect(p.name) AS names", nil)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got, ok := rows[0][0].([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", rows[0][0])
	}
	if len(got) != 3 {
		t.Errorf("expected 3 names, got %v", got)
	}
}

func TestAggregateCountDistinct(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, c := range []string{"IT", "IT", "IT", "DE", "DE", "FR"} {
			if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"country": c}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (p:Person) RETURN count(DISTINCT p.country) AS c", nil)
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Errorf("got %v, want [[3]]", rows)
	}
}

func TestVarLengthExpand(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s) // Alice→Bob, Alice→Carol, Bob→Carol

	// 1..2 hops from Alice via KNOWS.
	// Depth 1: Bob, Carol.
	// Depth 2: from Alice→Bob→Carol → Carol (one more path).
	// Expected names: Bob, Carol, Carol.
	rows := runQuery(t, s,
		"MATCH (a:Person)-[:KNOWS*1..2]->(b:Person) WHERE a.email = $e RETURN b.name AS n ORDER BY n",
		map[string]any{"e": "a@b.com"})

	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	want := []string{"Bob", "Carol", "Carol"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestVarLengthTrailSemantics builds a 2-cycle a→b→a (two distinct edges) and
// verifies that *1..5 traversals never reuse the same edge twice. Reachable
// trails from a are: {b} via e1, {a} via e1+e2 — and nothing deeper since
// extending requires reusing e1 or e2.
func TestVarLengthTrailSemantics(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		a, err := graph.CreateNode(tx, []string{"N"}, map[string]any{"name": "a"})
		if err != nil {
			return err
		}
		b, err := graph.CreateNode(tx, []string{"N"}, map[string]any{"name": "b"})
		if err != nil {
			return err
		}
		if _, err := graph.CreateEdge(tx, "T", a, b, nil); err != nil {
			return err
		}
		_, err = graph.CreateEdge(tx, "T", b, a, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (a:N)-[:T*1..5]->(x) WHERE a.name = $n RETURN x.name AS n ORDER BY n",
		map[string]any{"n": "a"})
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	want := []string{"a", "b"} // exactly one trail reaching each
	if !equalStrings(got, want) {
		t.Errorf("trail semantics violated: got %v, want %v", got, want)
	}
}

func TestStringPredicates(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, name := range []string{"alice", "amber", "bob", "barbara"} {
			if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": name}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		where string
		want  []string
	}{
		{"starts-with", "p.name STARTS WITH 'a'", []string{"alice", "amber"}},
		{"ends-with", "p.name ENDS WITH 'a'", []string{"barbara"}},
		{"contains", "p.name CONTAINS 'ar'", []string{"barbara"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := runQuery(t, s,
				"MATCH (p:Person) WHERE "+tc.where+" RETURN p.name AS n ORDER BY n", nil)
			got := make([]string, len(rows))
			for i, r := range rows {
				got[i] = r[0].(string)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.where, got, tc.want)
			}
		})
	}
}

func TestIsNullAndNot(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "A", "email": "a@x.com"}); err != nil {
			return err
		}
		if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "B"}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (p:Person) WHERE p.email IS NULL RETURN p.name AS n", nil)
	if len(rows) != 1 || rows[0][0] != "B" {
		t.Errorf("IS NULL: got %v, want [[B]]", rows)
	}
	rows = runQuery(t, s,
		"MATCH (p:Person) WHERE p.email IS NOT NULL RETURN p.name AS n", nil)
	if len(rows) != 1 || rows[0][0] != "A" {
		t.Errorf("IS NOT NULL: got %v, want [[A]]", rows)
	}
}

func TestInListLiteral(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, c := range []string{"IT", "DE", "FR", "JP"} {
			if _, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"c": c}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (p:P) WHERE p.c IN ['IT', 'FR'] RETURN p.c AS c ORDER BY c", nil)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	if want := []string{"FR", "IT"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCaseExpressions(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, age := range []int64{12, 30, 70} {
			if _, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"age": age}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Searched form
	rows := runQuery(t, s,
		"MATCH (p:P) RETURN CASE WHEN p.age < 18 THEN 'minor' WHEN p.age < 65 THEN 'adult' ELSE 'senior' END AS band ORDER BY p.age", nil)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	if want := []string{"minor", "adult", "senior"}; !equalStrings(got, want) {
		t.Errorf("searched: got %v, want %v", got, want)
	}

	// Simple form
	rows = runQuery(t, s,
		"MATCH (p:P) WHERE p.age = 30 RETURN CASE p.age WHEN 30 THEN 'match' ELSE 'no' END AS r", nil)
	if len(rows) != 1 || rows[0][0] != "match" {
		t.Errorf("simple: got %v", rows)
	}
}

func TestScalarFunctions(t *testing.T) {
	s := newStore(t)
	var aliceID, knowsID uint64
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		aliceID, err = graph.CreateNode(tx, []string{"Person", "Admin"}, map[string]any{"name": "Alice"})
		if err != nil {
			return err
		}
		bob, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Bob"})
		if err != nil {
			return err
		}
		knowsID, err = graph.CreateEdge(tx, "KNOWS", aliceID, bob, map[string]any{"since": int64(2020)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// id() on node and edge.
	rows := runQuery(t, s, "MATCH (p:Person {name: 'Alice'}) RETURN id(p) AS i", nil)
	if len(rows) != 1 || rows[0][0] != int64(aliceID) {
		t.Errorf("id(node): got %v", rows)
	}
	rows = runQuery(t, s, "MATCH (a)-[r:KNOWS]->(b) RETURN id(r) AS i", nil)
	if len(rows) != 1 || rows[0][0] != int64(knowsID) {
		t.Errorf("id(rel): got %v", rows)
	}

	// labels() returns both labels in some order — check membership.
	rows = runQuery(t, s, "MATCH (p:Person {name: 'Alice'}) RETURN labels(p) AS ls", nil)
	if len(rows) != 1 {
		t.Fatalf("labels: %v", rows)
	}
	ls := rows[0][0].([]any)
	got := map[string]bool{}
	for _, v := range ls {
		got[v.(string)] = true
	}
	if !got["Person"] || !got["Admin"] {
		t.Errorf("labels = %v, want Person and Admin", ls)
	}

	// type() returns the rel type.
	rows = runQuery(t, s, "MATCH (a)-[r:KNOWS]->(b) RETURN type(r) AS t", nil)
	if len(rows) != 1 || rows[0][0] != "KNOWS" {
		t.Errorf("type(): %v", rows)
	}

	// String functions
	rows = runQuery(t, s, "RETURN toUpper('hello') AS u, toLower('WORLD') AS l, trim('  x  ') AS t, substring('abcdef', 1, 3) AS s, size('αβγ') AS n", nil)
	if len(rows) != 1 {
		t.Fatalf("string fns: %v", rows)
	}
	row := rows[0]
	if row[0] != "HELLO" || row[1] != "world" || row[2] != "x" || row[3] != "bcd" || row[4] != int64(3) {
		t.Errorf("string fns row = %v", row)
	}

	// Conversions
	rows = runQuery(t, s, "RETURN toInteger('42') AS i, toFloat('3.5') AS f, toString(7) AS s", nil)
	row = rows[0]
	if row[0] != int64(42) || row[1] != 3.5 || row[2] != "7" {
		t.Errorf("conversions: %v", row)
	}

	// List helpers
	rows = runQuery(t, s, "RETURN size([1,2,3]) AS n, head([10,20,30]) AS h, last([10,20,30]) AS l, tail([10,20,30]) AS t", nil)
	row = rows[0]
	if row[0] != int64(3) || row[1] != int64(10) || row[2] != int64(30) {
		t.Errorf("list size/head/last: %v", row)
	}
	tail := row[3].([]any)
	if len(tail) != 2 || tail[0] != int64(20) || tail[1] != int64(30) {
		t.Errorf("list tail: %v", tail)
	}
}

func TestOptionalMatchProducesNullForUnmatched(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		a, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "A"})
		if err != nil {
			return err
		}
		b, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "B"})
		if err != nil {
			return err
		}
		if _, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "C"}); err != nil {
			return err
		}
		_, err = graph.CreateEdge(tx, "KNOWS", a, b, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a.name AS an, b.name AS bn ORDER BY an", nil)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (one per person), got %d (%v)", len(rows), rows)
	}
	// A -> B; B has no outgoing; C has no outgoing.
	if rows[0][0] != "A" || rows[0][1] != "B" {
		t.Errorf("row[0] = %v, want [A B]", rows[0])
	}
	if rows[1][0] != "B" || rows[1][1] != nil {
		t.Errorf("row[1] = %v, want [B null]", rows[1])
	}
	if rows[2][0] != "C" || rows[2][1] != nil {
		t.Errorf("row[2] = %v, want [C null]", rows[2])
	}
}

func TestUnwindList(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "UNWIND [1, 2, 3] AS x RETURN x", nil)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %v", rows)
	}
	for i, want := range []int64{1, 2, 3} {
		if rows[i][0] != want {
			t.Errorf("row[%d] = %v, want %d", i, rows[i][0], want)
		}
	}
}

func TestSetLabels(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "Alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runQuery(t, s, "MATCH (n:Person) SET n:Admin:Manager", nil)
	rows := runQuery(t, s, "MATCH (n:Admin) RETURN n.name AS n", nil)
	if len(rows) != 1 || rows[0][0] != "Alice" {
		t.Errorf("expected Alice with :Admin: %v", rows)
	}
	rows = runQuery(t, s, "MATCH (n:Manager) RETURN n.name AS n", nil)
	if len(rows) != 1 || rows[0][0] != "Alice" {
		t.Errorf("expected Alice with :Manager: %v", rows)
	}
}

func TestSetMapReplaceAndMerge(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice", "age": int64(30)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// SET n += {age: 31, country: 'IT'} merges.
	runQuery(t, s, "MATCH (n:P {name: 'Alice'}) SET n += {age: 31, country: 'IT'}", nil)
	rows := runQuery(t, s, "MATCH (n:P) RETURN n.name AS name, n.age AS age, n.country AS c", nil)
	if rows[0][0] != "Alice" || rows[0][1] != int64(31) || rows[0][2] != "IT" {
		t.Errorf("after +=: %v", rows[0])
	}

	// SET n = {name: 'Alice', score: 99} replaces (age/country gone).
	runQuery(t, s, "MATCH (n:P {name: 'Alice'}) SET n = {name: 'Alice', score: 99}", nil)
	rows = runQuery(t, s, "MATCH (n:P) RETURN n.name AS name, n.score AS s, n.age AS age", nil)
	if rows[0][0] != "Alice" || rows[0][1] != int64(99) || rows[0][2] != nil {
		t.Errorf("after =: %v", rows[0])
	}
}

func TestRemovePropertyAndLabel(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P", "Admin"}, map[string]any{"name": "Alice", "email": "a@x.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runQuery(t, s, "MATCH (n:P) REMOVE n.email, n:Admin", nil)
	rows := runQuery(t, s, "MATCH (n:P) RETURN n.email AS e", nil)
	if rows[0][0] != nil {
		t.Errorf("email should be removed: %v", rows[0])
	}
	rows = runQuery(t, s, "MATCH (n:Admin) RETURN n.name AS n", nil)
	if len(rows) != 0 {
		t.Errorf("Admin label should be removed: %v", rows)
	}
}

func TestMergeOnCreateAndOnMatch(t *testing.T) {
	s := newStore(t)
	// First MERGE: creates → OnCreate fires.
	runQuery(t, s,
		"MERGE (n:Person {email: 'a@x.com'}) ON CREATE SET n.created = 1 ON MATCH SET n.seen = 1 RETURN n.email AS e",
		nil)
	rows := runQuery(t, s,
		"MATCH (n:Person {email: 'a@x.com'}) RETURN n.created AS c, n.seen AS s", nil)
	if rows[0][0] != int64(1) || rows[0][1] != nil {
		t.Errorf("first MERGE expected created=1 seen=null, got %v", rows[0])
	}

	// Second MERGE: matches → OnMatch fires.
	runQuery(t, s,
		"MERGE (n:Person {email: 'a@x.com'}) ON CREATE SET n.created = 1 ON MATCH SET n.seen = 1",
		nil)
	rows = runQuery(t, s,
		"MATCH (n:Person {email: 'a@x.com'}) RETURN n.created AS c, n.seen AS s", nil)
	if rows[0][0] != int64(1) || rows[0][1] != int64(1) {
		t.Errorf("second MERGE expected created=1 seen=1, got %v", rows[0])
	}
}

func TestUnionDistinctDeduplicates(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, p := range [][2]string{
			{"Person", "Alice"}, {"Person", "Bob"},
			{"Company", "Acme"}, {"Company", "Alice"},
		} {
			if _, err := graph.CreateNode(tx, []string{p[0]}, map[string]any{"name": p[1]}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// UNION dedup: Alice appears once even though both arms yield it.
	rows := runQuery(t, s,
		"MATCH (p:Person) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n", nil)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	sort.Strings(got)
	if want := []string{"Acme", "Alice", "Bob"}; !equalStrings(got, want) {
		t.Errorf("UNION: got %v, want %v", got, want)
	}

	// UNION ALL keeps duplicates.
	rows = runQuery(t, s,
		"MATCH (p:Person) RETURN p.name AS n UNION ALL MATCH (c:Company) RETURN c.name AS n", nil)
	got = got[:0]
	for _, r := range rows {
		got = append(got, r[0].(string))
	}
	sort.Strings(got)
	if want := []string{"Acme", "Alice", "Alice", "Bob"}; !equalStrings(got, want) {
		t.Errorf("UNION ALL: got %v, want %v", got, want)
	}
}

func TestOptionalMatchWithFailingWhere(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		a, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "A"})
		if err != nil {
			return err
		}
		b, err := graph.CreateNode(tx, []string{"Person"}, map[string]any{"name": "B", "score": int64(10)})
		if err != nil {
			return err
		}
		_, err = graph.CreateEdge(tx, "T", a, b, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// b.score = 10 < 100, so even though the MATCH succeeds, the WHERE
	// filters the row to a null binding for b.
	rows := runQuery(t, s,
		"MATCH (a:Person {name: 'A'}) OPTIONAL MATCH (a)-[:T]->(b) WHERE b.score > 100 RETURN a.name AS an, b AS bn", nil)
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %v", rows)
	}
	if rows[0][0] != "A" || rows[0][1] != nil {
		t.Errorf("row = %v, want [A null]", rows[0])
	}
}

func TestOptionalMatchChained(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		a, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "A"})
		if err != nil {
			return err
		}
		b, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "B"})
		if err != nil {
			return err
		}
		c, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "C"})
		if err != nil {
			return err
		}
		if _, err := graph.CreateEdge(tx, "T", a, b, nil); err != nil {
			return err
		}
		_, err = graph.CreateEdge(tx, "T", c, a, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rows := runQuery(t, s,
		"MATCH (a:P {name: 'A'}) OPTIONAL MATCH (a)-[:T]->(b) OPTIONAL MATCH (c)-[:T]->(a) RETURN b.name AS bn, c.name AS cn", nil)
	if len(rows) != 1 || rows[0][0] != "B" || rows[0][1] != "C" {
		t.Errorf("row = %v, want [B C]", rows[0])
	}
}

func TestUnwindEmpty(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "UNWIND [] AS x RETURN x", nil)
	if len(rows) != 0 {
		t.Errorf("expected zero rows, got %v", rows)
	}
}

func TestUnwindNull(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "WITH null AS x UNWIND x AS y RETURN y", nil)
	if len(rows) != 0 {
		t.Errorf("expected zero rows, got %v", rows)
	}
}

func TestRemoveMissingPropertyIsNoOp(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Property "email" was never set; REMOVE must succeed silently.
	runQuery(t, s, "MATCH (n:P) REMOVE n.email", nil)
	rows := runQuery(t, s, "MATCH (n:P) RETURN n.name AS name", nil)
	if len(rows) != 1 || rows[0][0] != "Alice" {
		t.Errorf("unexpected rows after no-op REMOVE: %v", rows)
	}
}

func TestRemoveLabelNotPresentIsNoOp(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runQuery(t, s, "MATCH (n:P) REMOVE n:DoesNotExist", nil)
	rows := runQuery(t, s, "MATCH (n:P) RETURN n.name AS name", nil)
	if len(rows) != 1 {
		t.Errorf("unexpected rows after no-op REMOVE label: %v", rows)
	}
}

func TestMergeOnCreateOnlyOnFirstCall(t *testing.T) {
	s := newStore(t)
	runQuery(t, s,
		"MERGE (n:P {email: 'x'}) ON CREATE SET n.created = 1 ON MATCH SET n.seen = 1", nil)
	runQuery(t, s,
		"MERGE (n:P {email: 'x'}) ON CREATE SET n.created = 2 ON MATCH SET n.seen = 1", nil)
	rows := runQuery(t, s,
		"MATCH (n:P {email: 'x'}) RETURN n.created AS c, n.seen AS s", nil)
	if len(rows) != 1 || rows[0][0] != int64(1) || rows[0][1] != int64(1) {
		t.Errorf("expected created=1 seen=1 after two MERGE, got %v", rows[0])
	}
}

func TestCaseNoMatchNoElseReturnsNull(t *testing.T) {
	s := newStore(t)
	rows := runQuery(t, s, "RETURN CASE WHEN false THEN 1 END AS v", nil)
	if len(rows) != 1 || rows[0][0] != nil {
		t.Errorf("expected [null], got %v", rows)
	}
}

func TestStringPredicateWithNullOperand(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice"})
		if err != nil {
			return err
		}
		// no `name` here → property is null.
		_, err = graph.CreateNode(tx, []string{"P"}, map[string]any{"email": "x"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// null operand → predicate is null → row excluded.
	rows := runQuery(t, s, "MATCH (n:P) WHERE n.name STARTS WITH 'A' RETURN n.name AS name", nil)
	if len(rows) != 1 || rows[0][0] != "Alice" {
		t.Errorf("expected only Alice, got %v", rows)
	}
}

func TestInWithNullList(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// IN against a null list → null → row filtered out.
	rows := runQuery(t, s, "MATCH (n:P) WHERE n.name IN null RETURN n.name", nil)
	if len(rows) != 0 {
		t.Errorf("expected zero rows, got %v", rows)
	}
}

func TestAggregationSkipsNulls(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		// Two nodes with score, one without (null).
		if _, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"score": int64(10)}); err != nil {
			return err
		}
		if _, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"score": int64(30)}); err != nil {
			return err
		}
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "noscore"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rows := runQuery(t, s,
		"MATCH (n:P) RETURN count(n.score) AS c, sum(n.score) AS s, avg(n.score) AS a, min(n.score) AS mn, max(n.score) AS mx", nil)
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %v", rows)
	}
	r := rows[0]
	if r[0] != int64(2) || r[1] != int64(40) || r[2] != float64(20) || r[3] != int64(10) || r[4] != int64(30) {
		t.Errorf("aggregates ignoring null = %v, want [2 40 20 10 30]", r)
	}
}

func TestIdAndLabelsOnNull(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "A"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// b is always null (no relation exists), so id(b) and labels(b) must be null.
	rows := runQuery(t, s,
		"MATCH (a:P) OPTIONAL MATCH (a)-[:T]->(b) RETURN id(b) AS ib, labels(b) AS lb", nil)
	if len(rows) != 1 || rows[0][0] != nil || rows[0][1] != nil {
		t.Errorf("row = %v, want [null null]", rows[0])
	}
}

func TestVarLengthZeroHops(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		a, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "A"})
		if err != nil {
			return err
		}
		b, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "B"})
		if err != nil {
			return err
		}
		_, err = graph.CreateEdge(tx, "T", a, b, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// *0..1 from A yields A (zero hops) and B (one hop).
	rows := runQuery(t, s,
		"MATCH (a:P {name: 'A'})-[:T*0..1]->(b) RETURN b.name AS bn ORDER BY bn", nil)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r[0].(string))
	}
	if want := []string{"A", "B"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSetPropertyToNullRemovesIt(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		_, err := graph.CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice", "age": int64(30)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// SET n.age = null must remove the key, per Cypher semantics.
	runQuery(t, s, "MATCH (n:P {name: 'Alice'}) SET n.age = null", nil)
	rows := runQuery(t, s, "MATCH (n:P) RETURN keys(n) AS ks", nil)
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %v", rows)
	}
	ks, ok := rows[0][0].([]any)
	if !ok {
		t.Fatalf("expected keys(n) to be a list, got %T", rows[0][0])
	}
	for _, k := range ks {
		if k == "age" {
			t.Errorf("age should be absent after SET n.age = null, got keys %v", ks)
		}
	}
}

func TestConcurrentMergeIsIdempotent(t *testing.T) {
	s := newStore(t)
	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- nil
				}
			}()
			q, err := parser.Parse("MERGE (n:Person {email: 'x@x.com'}) RETURN n.email")
			if err != nil {
				errs <- err
				return
			}
			res, err := sema.Analyze(q)
			if err != nil {
				errs <- err
				return
			}
			err = s.Update(func(txn storage.Txn) error {
				p, err := plan.Plan(q, testCatalog{txn: txn})
				if err != nil {
					return err
				}
				_, err = Run(p, res.Columns, &Context{Txn: txn})
				return err
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MERGE: %v", err)
		}
	}

	rows := runQuery(t, s, "MATCH (n:Person {email: 'x@x.com'}) RETURN n.email AS e", nil)
	if len(rows) != 1 {
		t.Errorf("expected exactly one Person, got %d (%v)", len(rows), rows)
	}
}

func TestLimit(t *testing.T) {
	s := newStore(t)
	seedSocial(t, s)
	rows := runQuery(t, s, "MATCH (p:Person) RETURN p.name AS name ORDER BY name LIMIT 2", nil)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r[0].(string)
	}
	if want := []string{"Alice", "Bob"}; !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
