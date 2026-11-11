package changefeed

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/giovibal/intreccio/internal/graph"
	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// commit runs fn through a Capture in one write transaction and returns the
// resulting ChangeSet, mirroring what a commit point does.
func commit(t *testing.T, s storage.Store, fn func(txn storage.Txn) error) *ChangeSet {
	t.Helper()
	var cs *ChangeSet
	err := s.Update(func(txn storage.Txn) error {
		c := NewCapture(txn)
		if err := fn(c); err != nil {
			return err
		}
		var err error
		cs, err = c.ChangeSet()
		return err
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return cs
}

func TestCaptureNodeLifecycle(t *testing.T) {
	s := newStore(t)

	var id uint64
	cs := commit(t, s, func(txn storage.Txn) error {
		var err error
		id, err = graph.CreateNode(txn, []string{"Person"}, map[string]any{"name": "Ann"})
		return err
	})
	if len(cs.Nodes) != 1 || len(cs.Edges) != 0 {
		t.Fatalf("create: %+v", cs)
	}
	if c := cs.Nodes[0]; c.ID != id || c.Before != nil || c.After == nil || len(c.After.Labels) != 1 || len(c.After.Props) != 1 {
		t.Fatalf("create change = %+v", c)
	}

	cs = commit(t, s, func(txn storage.Txn) error {
		if err := graph.SetProperty(txn, id, "age", 30); err != nil {
			return err
		}
		return graph.SetProperty(txn, id, "age", 31) // two writes, one change
	})
	if len(cs.Nodes) != 1 {
		t.Fatalf("update: %+v", cs)
	}
	if c := cs.Nodes[0]; c.Before == nil || c.After == nil || len(c.Before.Props) != 1 || len(c.After.Props) != 2 {
		t.Fatalf("update change = %+v", c)
	}

	cs = commit(t, s, func(txn storage.Txn) error { return graph.DeleteNode(txn, id) })
	if len(cs.Nodes) != 1 {
		t.Fatalf("delete: %+v", cs)
	}
	if c := cs.Nodes[0]; c.Before == nil || c.After != nil || len(c.Before.Props) != 2 {
		t.Fatalf("delete change = %+v", c)
	}
}

func TestCaptureEdgeLifecycle(t *testing.T) {
	s := newStore(t)
	var a, b, e uint64
	commit(t, s, func(txn storage.Txn) error {
		var err error
		if a, err = graph.CreateNode(txn, nil, nil); err != nil {
			return err
		}
		b, err = graph.CreateNode(txn, nil, nil)
		return err
	})

	cs := commit(t, s, func(txn storage.Txn) error {
		var err error
		e, err = graph.CreateEdge(txn, "KNOWS", a, b, map[string]any{"since": 2020})
		return err
	})
	if len(cs.Nodes) != 0 || len(cs.Edges) != 1 {
		t.Fatalf("create edge: %+v", cs)
	}
	if c := cs.Edges[0]; c.ID != e || c.Before != nil || c.After == nil || c.After.Src != a || c.After.Dst != b {
		t.Fatalf("edge create change = %+v", c)
	}

	// DETACH DELETE reports the node and its incident relationship.
	cs = commit(t, s, func(txn storage.Txn) error { return graph.DetachDeleteNode(txn, a) })
	if len(cs.Nodes) != 1 || len(cs.Edges) != 1 {
		t.Fatalf("detach delete: %+v", cs)
	}
	if c := cs.Edges[0]; c.ID != e || c.Before == nil || c.After != nil {
		t.Fatalf("edge delete change = %+v", c)
	}
}

// TestCaptureNetNoOps checks that work which leaves a record as it was reports
// no change: create-then-delete in one transaction, and rewriting equal values.
func TestCaptureNetNoOps(t *testing.T) {
	s := newStore(t)
	cs := commit(t, s, func(txn storage.Txn) error {
		id, err := graph.CreateNode(txn, []string{"Tmp"}, map[string]any{"k": 1})
		if err != nil {
			return err
		}
		return graph.DeleteNode(txn, id)
	})
	if !cs.Empty() {
		t.Errorf("create+delete: %+v, want empty", cs)
	}

	var id uint64
	commit(t, s, func(txn storage.Txn) error {
		var err error
		id, err = graph.CreateNode(txn, []string{"P"}, map[string]any{"k": 1})
		return err
	})
	cs = commit(t, s, func(txn storage.Txn) error {
		if err := graph.SetProperty(txn, id, "k", 1); err != nil {
			return err
		}
		return graph.AddLabels(txn, id, []string{"P"})
	})
	if !cs.Empty() {
		t.Errorf("rewriting equal values: %+v, want empty", cs)
	}

	// A change that is undone within the same transaction is not a change.
	cs = commit(t, s, func(txn storage.Txn) error {
		if err := graph.SetProperty(txn, id, "k", 2); err != nil {
			return err
		}
		return graph.SetProperty(txn, id, "k", 1)
	})
	if !cs.Empty() {
		t.Errorf("change undone in the transaction: %+v, want empty", cs)
	}
}

func TestCaptureIgnoresIndexAndCatalogKeys(t *testing.T) {
	s := newStore(t)
	cs := commit(t, s, func(txn storage.Txn) error {
		// Short and long keys under the tracked tags, plus other tables.
		for _, k := range [][]byte{{codec.KeyNode}, {codec.KeyEdge, 1, 2}, append([]byte{codec.KeyNode}, make([]byte, 20)...),
			codec.LabelKey(1, 1), codec.OutKey(1, 1, 2, 1), []byte("Lname"), []byte("c")} {
			if err := txn.Set(k, []byte{1}); err != nil {
				return err
			}
		}
		return nil
	})
	if !cs.Empty() {
		t.Errorf("non-record keys: %+v, want empty", cs)
	}
}

func TestCaptureReportsUndecodableRecord(t *testing.T) {
	s := newStore(t)
	err := s.Update(func(txn storage.Txn) error {
		c := NewCapture(txn)
		if err := c.Set(codec.NodeKey(1), []byte{0xff, 0xff, 0xff}); err != nil {
			return err
		}
		_, err := c.ChangeSet()
		return err
	})
	if err == nil {
		t.Fatal("ChangeSet over a corrupt record: want an error")
	}
}

// shadow is an independent model of the logical state, maintained only from
// ChangeSets.
type shadow struct {
	nodes map[uint64]codec.NodeRecord
	edges map[uint64]codec.EdgeRecord
}

// apply folds cs into the model, checking that every before-image is exactly
// what the model held.
func (sh *shadow) apply(cs *ChangeSet) error {
	for _, c := range cs.Nodes {
		cur, ok := sh.nodes[c.ID]
		switch {
		case c.Before == nil && ok:
			return errors.New("node reported as created but the model has it")
		case c.Before != nil && !ok:
			return errors.New("node before-image for a node the model lacks")
		case c.Before != nil && !reflect.DeepEqual(*c.Before, cur):
			return errors.New("node before-image differs from the model")
		case c.Before == nil && c.After == nil:
			return errors.New("node change with neither image")
		}
		if c.After == nil {
			delete(sh.nodes, c.ID)
		} else {
			sh.nodes[c.ID] = *c.After
		}
	}
	for _, c := range cs.Edges {
		cur, ok := sh.edges[c.ID]
		switch {
		case c.Before == nil && ok:
			return errors.New("edge reported as created but the model has it")
		case c.Before != nil && !ok:
			return errors.New("edge before-image for an edge the model lacks")
		case c.Before != nil && !reflect.DeepEqual(*c.Before, cur):
			return errors.New("edge before-image differs from the model")
		case c.Before == nil && c.After == nil:
			return errors.New("edge change with neither image")
		}
		if c.After == nil {
			delete(sh.edges, c.ID)
		} else {
			sh.edges[c.ID] = *c.After
		}
	}
	return nil
}

// actual reads the logical state straight from the record tables.
func actual(s storage.Store) (*shadow, error) {
	out := &shadow{nodes: map[uint64]codec.NodeRecord{}, edges: map[uint64]codec.EdgeRecord{}}
	err := s.View(func(txn storage.Txn) error {
		ids, err := graph.AllNodes(txn)
		if err != nil {
			return err
		}
		for _, id := range ids {
			n, err := graph.GetNode(txn, id)
			if err != nil {
				return err
			}
			out.nodes[id] = n.NodeRecord
			es, err := graph.OutEdges(txn, id, 0)
			if err != nil {
				return err
			}
			for _, ref := range es {
				e, err := graph.GetEdge(txn, ref.ID)
				if err != nil {
					return err
				}
				out.edges[ref.ID] = e.EdgeRecord
			}
		}
		return nil
	})
	return out, err
}

var errRollback = errors.New("rollback")

// randomOp applies one random graph mutation. Errors such as "node has edges"
// are part of the exercise: the caller treats them as a failed statement.
func randomOp(r *rand.Rand, txn storage.Txn) error {
	labels := []string{"A", "B", "C"}
	keys := []string{"k1", "k2", "k3"}
	types := []string{"T1", "T2"}
	pickNode := func() (uint64, bool, error) {
		ids, err := graph.AllNodes(txn)
		if err != nil || len(ids) == 0 {
			return 0, false, err
		}
		return ids[r.Intn(len(ids))], true, nil
	}
	val := func() any {
		switch r.Intn(4) {
		case 0:
			return int64(r.Intn(3))
		case 1:
			return []string{"x", "y"}[r.Intn(2)]
		case 2:
			return r.Intn(2) == 0
		default:
			return []any{int64(r.Intn(2)), "l"}
		}
	}
	id, ok, err := pickNode()
	if err != nil {
		return err
	}
	op := r.Intn(10)
	if !ok {
		op = 0
	}
	switch op {
	case 0, 1:
		var ls []string
		for _, l := range labels {
			if r.Intn(3) == 0 {
				ls = append(ls, l)
			}
		}
		props := map[string]any{}
		for _, k := range keys {
			if r.Intn(2) == 0 {
				props[k] = val()
			}
		}
		_, err := graph.CreateNode(txn, ls, props)
		return err
	case 2:
		return graph.SetProperty(txn, id, keys[r.Intn(len(keys))], val())
	case 3:
		return graph.RemoveProperty(txn, id, keys[r.Intn(len(keys))])
	case 4:
		return graph.AddLabels(txn, id, []string{labels[r.Intn(len(labels))]})
	case 5:
		return graph.RemoveLabels(txn, id, []string{labels[r.Intn(len(labels))]})
	case 6:
		return graph.SetNodeProperties(txn, id, map[string]any{keys[r.Intn(len(keys))]: val()}, r.Intn(2) == 0)
	case 7:
		other, _, err := pickNode()
		if err != nil {
			return err
		}
		_, err = graph.CreateEdge(txn, types[r.Intn(len(types))], id, other, map[string]any{"w": val()})
		return err
	case 8:
		es, err := graph.OutEdges(txn, id, 0)
		if err != nil || len(es) == 0 {
			return err
		}
		return graph.DeleteEdge(txn, es[r.Intn(len(es))].ID)
	default:
		if r.Intn(2) == 0 {
			return graph.DetachDeleteNode(txn, id)
		}
		return graph.DeleteNode(txn, id) // fails when the node has edges
	}
}

// TestCaptureRebuildsState is the property test of the capture: folding the
// ChangeSets of a random sequence of committed transactions into a model yields
// exactly the state in the store, and rolled-back transactions contribute
// nothing.
func TestCaptureRebuildsState(t *testing.T) {
	prop := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		s, err := badgerstore.OpenInMemory()
		if err != nil {
			t.Logf("open: %v", err)
			return false
		}
		defer func() { _ = s.Close() }()
		model := &shadow{nodes: map[uint64]codec.NodeRecord{}, edges: map[uint64]codec.EdgeRecord{}}

		for range 40 {
			var cs *ChangeSet
			rollback := r.Intn(6) == 0
			err := s.Update(func(txn storage.Txn) error {
				c := NewCapture(txn)
				for range 1 + r.Intn(4) {
					if err := randomOp(r, c); err != nil {
						return err
					}
				}
				if rollback {
					return errRollback
				}
				var err error
				cs, err = c.ChangeSet()
				return err
			})
			if err != nil {
				continue // failed or rolled-back statement: nothing is published
			}
			if err := model.apply(cs); err != nil {
				t.Logf("seed %d: %v", seed, err)
				return false
			}
		}

		got, err := actual(s)
		if err != nil {
			t.Logf("read back: %v", err)
			return false
		}
		if !reflect.DeepEqual(model, got) {
			t.Logf("seed %d: model and store diverge:\nmodel=%+v\nstore=%+v", seed, model, got)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 60}); err != nil {
		t.Error(err)
	}
}
