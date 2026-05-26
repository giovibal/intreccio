package exec

import (
	"sort"
	"testing"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/parser"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/cypher/sema"
	"github.com/giovibal/mycypher/internal/graph"
	"github.com/giovibal/mycypher/internal/storage"
	badgerstore "github.com/giovibal/mycypher/internal/storage/badger"
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
	var rows [][]any
	err = s.View(func(txn storage.Txn) error {
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
