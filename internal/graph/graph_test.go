package graph

import (
	"sort"
	"testing"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/storage"
	badgeradapter "github.com/giovibal/mycypher/internal/storage/badger"
)

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := badgeradapter.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustUpdate(t *testing.T, s storage.Store, fn func(storage.Txn) error) {
	t.Helper()
	if err := s.Update(fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func mustView(t *testing.T, s storage.Store, fn func(storage.Txn) error) {
	t.Helper()
	if err := s.View(fn); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestCreateNodeAppearsInLabelScan(t *testing.T) {
	s := newStore(t)
	var personID uint32
	var n1, n2 uint64

	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		if n1, err = CreateNode(tx, []string{"Person"}, map[string]any{"name": "Alice"}); err != nil {
			return err
		}
		if n2, err = CreateNode(tx, []string{"Person"}, map[string]any{"name": "Bob"}); err != nil {
			return err
		}
		if _, err = CreateNode(tx, []string{"Company"}, nil); err != nil {
			return err
		}
		personID, _, err = catalog.LookupLabel(tx, "Person")
		return err
	})

	mustView(t, s, func(tx storage.Txn) error {
		got, err := NodesByLabel(tx, personID)
		if err != nil {
			return err
		}
		if !equalSet(got, []uint64{n1, n2}) {
			t.Errorf("NodesByLabel(Person) = %v, want %v", got, []uint64{n1, n2})
		}
		return nil
	})
}

func TestEdgeAppearsInBothAdjacencies(t *testing.T) {
	s := newStore(t)
	var a, b, edge uint64
	var knows uint32

	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		if a, err = CreateNode(tx, []string{"Person"}, nil); err != nil {
			return err
		}
		if b, err = CreateNode(tx, []string{"Person"}, nil); err != nil {
			return err
		}
		if edge, err = CreateEdge(tx, "KNOWS", a, b, map[string]any{"since": int64(2020)}); err != nil {
			return err
		}
		knows, _, err = catalog.LookupType(tx, "KNOWS")
		return err
	})

	mustView(t, s, func(tx storage.Txn) error {
		out, err := OutEdges(tx, a, knows)
		if err != nil {
			return err
		}
		if len(out) != 1 || out[0].ID != edge || out[0].Dst != b || out[0].Src != a {
			t.Errorf("OutEdges(a) = %+v", out)
		}
		in, err := InEdges(tx, b, knows)
		if err != nil {
			return err
		}
		if len(in) != 1 || in[0].ID != edge || in[0].Src != a || in[0].Dst != b {
			t.Errorf("InEdges(b) = %+v", in)
		}
		// typeID 0 = any type.
		anyOut, err := OutEdges(tx, a, 0)
		if err != nil {
			return err
		}
		if len(anyOut) != 1 {
			t.Errorf("OutEdges(a, any) = %+v", anyOut)
		}
		// The edge record round-trips its properties.
		e, err := GetEdge(tx, edge)
		if err != nil {
			return err
		}
		sinceID, _, _ := catalog.LookupKey(tx, "since")
		if e.Src != a || e.Dst != b || e.Type != knows || e.Props[sinceID] != int64(2020) {
			t.Errorf("GetEdge = %+v", e)
		}
		return nil
	})
}

func TestSetProperty(t *testing.T) {
	s := newStore(t)
	var n uint64
	var ageID uint32
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		if n, err = CreateNode(tx, []string{"Person"}, map[string]any{"name": "Alice"}); err != nil {
			return err
		}
		if err = SetProperty(tx, n, "age", 30); err != nil { // int -> int64
			return err
		}
		if err = SetProperty(tx, n, "age", int64(31)); err != nil { // overwrite
			return err
		}
		ageID, _, err = catalog.LookupKey(tx, "age")
		return err
	})
	mustView(t, s, func(tx storage.Txn) error {
		node, err := GetNode(tx, n)
		if err != nil {
			return err
		}
		if node.Props[ageID] != int64(31) {
			t.Errorf("age = %v (%T), want int64(31)", node.Props[ageID], node.Props[ageID])
		}
		return nil
	})
}

func TestNodesByPropertyIndexed(t *testing.T) {
	s := newStore(t)
	var alice uint64
	var personID, emailID uint32

	mustUpdate(t, s, func(tx storage.Txn) error {
		// The index must exist before the create so the `p` entries are written.
		pid, err := catalog.InternLabel(tx, "Person")
		if err != nil {
			return err
		}
		eid, err := catalog.InternKey(tx, "email")
		if err != nil {
			return err
		}
		personID, emailID = pid, eid
		if err := catalog.AddIndex(tx, pid, eid); err != nil {
			return err
		}
		if alice, err = CreateNode(tx, []string{"Person"}, map[string]any{"email": "a@b.com"}); err != nil {
			return err
		}
		if _, err = CreateNode(tx, []string{"Person"}, map[string]any{"email": "c@d.com"}); err != nil {
			return err
		}
		return nil
	})

	mustView(t, s, func(tx storage.Txn) error {
		got, err := NodesByProperty(tx, personID, emailID, "a@b.com")
		if err != nil {
			return err
		}
		if !equalSet(got, []uint64{alice}) {
			t.Errorf("NodesByProperty(email=a@b.com) = %v, want [%d]", got, alice)
		}
		none, err := NodesByProperty(tx, personID, emailID, "missing@x.com")
		if err != nil {
			return err
		}
		if len(none) != 0 {
			t.Errorf("expected no results, got %v", none)
		}
		return nil
	})
}

func TestNodesByPropertyFallback(t *testing.T) {
	s := newStore(t)
	var bob uint64
	var personID, ageID uint32
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		if _, err = CreateNode(tx, []string{"Person"}, map[string]any{"age": int64(20)}); err != nil {
			return err
		}
		if bob, err = CreateNode(tx, []string{"Person"}, map[string]any{"age": int64(40)}); err != nil {
			return err
		}
		personID, _, _ = catalog.LookupLabel(tx, "Person")
		ageID, _, err = catalog.LookupKey(tx, "age")
		return err
	})
	mustView(t, s, func(tx storage.Txn) error {
		// No index on (Person, age): use the fallback.
		if has, _ := catalog.HasIndex(tx, personID, ageID); has {
			t.Fatal("there should be no index")
		}
		got, err := NodesByProperty(tx, personID, ageID, 40) // int -> int64
		if err != nil {
			return err
		}
		if !equalSet(got, []uint64{bob}) {
			t.Errorf("fallback NodesByProperty(age=40) = %v, want [%d]", got, bob)
		}
		return nil
	})
}

// Invariant #1/#4: deleting an edge removes the `e` record and both adjacency
// views `o`/`i`.
func TestDeleteEdgeRemovesAdjacency(t *testing.T) {
	s := newStore(t)
	var a, b, edge uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		a, _ = CreateNode(tx, []string{"N"}, nil)
		b, _ = CreateNode(tx, []string{"N"}, nil)
		edge, err = CreateEdge(tx, "T", a, b, nil)
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error { return DeleteEdge(tx, edge) })

	mustView(t, s, func(tx storage.Txn) error {
		if _, err := GetEdge(tx, edge); err != ErrEdgeNotFound {
			t.Errorf("GetEdge after delete = %v, want ErrEdgeNotFound", err)
		}
		if out, _ := OutEdges(tx, a, 0); len(out) != 0 {
			t.Errorf("leftover OutEdges: %v", out)
		}
		if in, _ := InEdges(tx, b, 0); len(in) != 0 {
			t.Errorf("leftover InEdges: %v", in)
		}
		return nil
	})
}

// Invariant #1: deleting a node removes the `n` record and all `l`/`p` entries.
func TestDeleteNodeRemovesIndexes(t *testing.T) {
	s := newStore(t)
	var n uint64
	var personID, emailID uint32
	mustUpdate(t, s, func(tx storage.Txn) error {
		pid, _ := catalog.InternLabel(tx, "Person")
		eid, _ := catalog.InternKey(tx, "email")
		personID, emailID = pid, eid
		if err := catalog.AddIndex(tx, pid, eid); err != nil {
			return err
		}
		var err error
		n, err = CreateNode(tx, []string{"Person"}, map[string]any{"email": "x@y.com"})
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error { return DeleteNode(tx, n) })

	mustView(t, s, func(tx storage.Txn) error {
		if _, err := GetNode(tx, n); err != ErrNodeNotFound {
			t.Errorf("GetNode after delete = %v, want ErrNodeNotFound", err)
		}
		if got, _ := NodesByLabel(tx, personID); len(got) != 0 {
			t.Errorf("leftover `l` entries: %v", got)
		}
		if got, _ := NodesByProperty(tx, personID, emailID, "x@y.com"); len(got) != 0 {
			t.Errorf("leftover `p` entries: %v", got)
		}
		return nil
	})
}

func TestDeleteNodeWithEdgesFails(t *testing.T) {
	s := newStore(t)
	var a, b uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		a, _ = CreateNode(tx, []string{"N"}, nil)
		b, _ = CreateNode(tx, []string{"N"}, nil)
		_, err := CreateEdge(tx, "T", a, b, nil)
		return err
	})
	err := s.Update(func(tx storage.Txn) error { return DeleteNode(tx, a) })
	if err != ErrNodeHasEdges {
		t.Errorf("DeleteNode with edges = %v, want ErrNodeHasEdges", err)
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)
	mustView(t, s, func(tx storage.Txn) error {
		if _, err := GetNode(tx, 999); err != ErrNodeNotFound {
			t.Errorf("GetNode = %v, want ErrNodeNotFound", err)
		}
		if _, err := GetEdge(tx, 999); err != ErrEdgeNotFound {
			t.Errorf("GetEdge = %v, want ErrEdgeNotFound", err)
		}
		return nil
	})
}

func equalSet(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]uint64(nil), got...)
	w := append([]uint64(nil), want...)
	sort.Slice(g, func(i, j int) bool { return g[i] < g[j] })
	sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
