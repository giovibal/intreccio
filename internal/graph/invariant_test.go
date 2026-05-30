package graph

import (
	"bytes"
	"encoding/binary"
	"sort"
	"testing"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// assertNodeIndexConsistency verifies Invariante #1 for a single node: the base
// record agrees with every `l`/`p` index entry that mentions it, and vice
// versa. Any drift between record and index is a hard failure.
func assertNodeIndexConsistency(t *testing.T, txn storage.Txn, nodeID uint64) {
	t.Helper()
	node, err := GetNode(txn, nodeID)
	if err != nil {
		t.Fatalf("GetNode(%d): %v", nodeID, err)
	}

	// Record→index direction: every label in the record has an `l` entry.
	recordLabels := make(map[uint32]struct{}, len(node.Labels))
	for _, l := range node.Labels {
		recordLabels[l] = struct{}{}
		if _, err := txn.Get(codec.LabelKey(l, nodeID)); err != nil {
			t.Errorf("node %d: missing `l` entry for label %d: %v", nodeID, l, err)
		}
	}

	// Record→index: every indexed (label, key) with an indexable scalar value
	// must have a matching `p` entry.
	indexes, err := catalog.ListIndexes(txn)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	for _, idx := range indexes {
		if _, ok := recordLabels[idx.Label]; !ok {
			continue
		}
		v, hasProp := node.Props[idx.PropKey]
		if !hasProp || !isIndexable(v) {
			continue
		}
		pk, err := codec.PropKey(idx.Label, idx.PropKey, v, nodeID)
		if err != nil {
			t.Fatalf("PropKey: %v", err)
		}
		if _, err := txn.Get(pk); err != nil {
			t.Errorf("node %d: missing `p` entry for label=%d key=%d val=%v: %v",
				nodeID, idx.Label, idx.PropKey, v, err)
		}
	}

	// Index→record direction: scan all `l` entries for this node and confirm
	// the label is in the record.
	{
		it := txn.Scan([]byte{codec.KeyLabel})
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			label, owner, err := codec.ParseLabelKey(it.Key())
			if err != nil {
				t.Fatalf("ParseLabelKey: %v", err)
			}
			if owner != nodeID {
				continue
			}
			if _, ok := recordLabels[label]; !ok {
				t.Errorf("node %d: stray `l` entry for label %d (not in record)", nodeID, label)
			}
		}
	}

	// Index→record: scan all `p` entries pointing at this node and confirm
	// the (label, key, value) is reflected in the record.
	{
		it := txn.Scan([]byte{codec.KeyProp})
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			k := it.Key()
			owner, err := codec.ParsePropKeyNode(k)
			if err != nil {
				t.Fatalf("ParsePropKeyNode: %v", err)
			}
			if owner != nodeID {
				continue
			}
			label := binary.BigEndian.Uint32(k[1:5])
			propKey := binary.BigEndian.Uint32(k[5:9])
			if _, ok := recordLabels[label]; !ok {
				t.Errorf("node %d: stray `p` entry on label %d not present in record", nodeID, label)
				continue
			}
			rv, has := node.Props[propKey]
			if !has || !isIndexable(rv) {
				t.Errorf("node %d: stray `p` entry on key %d (record has has=%v val=%v)", nodeID, propKey, has, rv)
				continue
			}
			// Rebuild the canonical key for the recorded value: it must equal k.
			want, err := codec.PropKey(label, propKey, rv, nodeID)
			if err != nil {
				t.Fatalf("PropKey: %v", err)
			}
			if !bytes.Equal(k, want) {
				t.Errorf("node %d: `p` entry value differs from record (have %x want %x)", nodeID, k, want)
			}
		}
	}
}

// assertEdgeAdjacencyConsistency verifies the `o`/`i` pair exists for an edge.
func assertEdgeAdjacencyConsistency(t *testing.T, txn storage.Txn, edgeID uint64) {
	t.Helper()
	e, err := GetEdge(txn, edgeID)
	if err != nil {
		t.Fatalf("GetEdge(%d): %v", edgeID, err)
	}
	if _, err := txn.Get(codec.OutKey(e.Src, e.Type, e.Dst, e.ID)); err != nil {
		t.Errorf("edge %d: missing `o` entry: %v", edgeID, err)
	}
	if _, err := txn.Get(codec.InKey(e.Dst, e.Type, e.Src, e.ID)); err != nil {
		t.Errorf("edge %d: missing `i` entry: %v", edgeID, err)
	}
}

func TestAddLabelsKeepsIndexConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		nid, err = CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice", "age": int64(30)})
		if err != nil {
			return err
		}
		// Register an index on (Admin, name) so that AddLabels must populate it.
		lid, err := catalog.InternLabel(tx, "Admin")
		if err != nil {
			return err
		}
		kid, err := catalog.InternKey(tx, "name")
		if err != nil {
			return err
		}
		return catalog.AddIndex(tx, lid, kid)
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		return AddLabels(tx, nid, []string{"Admin", "Manager"})
	})
	mustView(t, s, func(tx storage.Txn) error {
		assertNodeIndexConsistency(t, tx, nid)
		return nil
	})
}

func TestRemoveLabelsKeepsIndexConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		lid, err := catalog.InternLabel(tx, "Admin")
		if err != nil {
			return err
		}
		kid, err := catalog.InternKey(tx, "name")
		if err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, kid); err != nil {
			return err
		}
		nid, err = CreateNode(tx, []string{"P", "Admin"}, map[string]any{"name": "Alice"})
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		return RemoveLabels(tx, nid, []string{"Admin"})
	})
	mustView(t, s, func(tx storage.Txn) error {
		assertNodeIndexConsistency(t, tx, nid)
		return nil
	})
}

func TestRemovePropertyKeepsIndexConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		lid, err := catalog.InternLabel(tx, "P")
		if err != nil {
			return err
		}
		kid, err := catalog.InternKey(tx, "email")
		if err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, kid); err != nil {
			return err
		}
		nid, err = CreateNode(tx, []string{"P"}, map[string]any{"email": "a@x.com", "name": "Alice"})
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		return RemoveProperty(tx, nid, "email")
	})
	mustView(t, s, func(tx storage.Txn) error {
		assertNodeIndexConsistency(t, tx, nid)
		return nil
	})
}

func TestSetNodePropertiesReplaceConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		lid, err := catalog.InternLabel(tx, "P")
		if err != nil {
			return err
		}
		kid, err := catalog.InternKey(tx, "name")
		if err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, kid); err != nil {
			return err
		}
		nid, err = CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice", "age": int64(30)})
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		return SetNodeProperties(tx, nid, map[string]any{"name": "Bob"}, true)
	})
	mustView(t, s, func(tx storage.Txn) error {
		assertNodeIndexConsistency(t, tx, nid)
		return nil
	})
}

func TestSetNodePropertiesMergeConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		lid, err := catalog.InternLabel(tx, "P")
		if err != nil {
			return err
		}
		kid, err := catalog.InternKey(tx, "name")
		if err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, kid); err != nil {
			return err
		}
		nid, err = CreateNode(tx, []string{"P"}, map[string]any{"name": "Alice", "age": int64(30)})
		return err
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		// Merge: name change, age unchanged, country added, then null wipes age.
		if err := SetNodeProperties(tx, nid, map[string]any{"name": "Carol", "country": "IT"}, false); err != nil {
			return err
		}
		return SetNodeProperties(tx, nid, map[string]any{"age": nil}, false)
	})
	mustView(t, s, func(tx storage.Txn) error {
		assertNodeIndexConsistency(t, tx, nid)
		n, err := GetNode(tx, nid)
		if err != nil {
			return err
		}
		// "age" should have been removed by the null merge.
		kid, found, err := catalog.LookupKey(tx, "age")
		if err != nil {
			return err
		}
		if found {
			if _, has := n.Props[kid]; has {
				t.Errorf("expected age to be removed, props=%v", n.Props)
			}
		}
		return nil
	})
}

func TestDetachDeleteSelfLoopConsistent(t *testing.T) {
	s := newStore(t)
	var nid uint64
	var eid uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		nid, err = CreateNode(tx, []string{"P"}, map[string]any{"name": "Solo"})
		if err != nil {
			return err
		}
		eid, err = CreateEdge(tx, "T", nid, nid, nil)
		return err
	})
	// Sanity: adjacency entries exist before delete.
	mustView(t, s, func(tx storage.Txn) error {
		assertEdgeAdjacencyConsistency(t, tx, eid)
		return nil
	})
	mustUpdate(t, s, func(tx storage.Txn) error {
		return DetachDeleteNode(tx, nid)
	})
	mustView(t, s, func(tx storage.Txn) error {
		// All `o`/`i`/`e` entries that mention nid must be gone.
		for _, prefix := range [][]byte{
			codec.OutPrefixAll(nid),
			codec.InPrefixAll(nid),
			codec.NodeKey(nid),
			codec.EdgeKey(eid),
		} {
			it := tx.Scan(prefix)
			if it.Valid() {
				t.Errorf("leftover entry under prefix %x after DETACH DELETE", prefix)
			}
			_ = it.Close()
		}
		return nil
	})
}

func TestConcurrentNodeCreates(t *testing.T) {
	s := newStore(t)
	const goroutines = 8
	const perGoroutine = 50
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < perGoroutine; i++ {
				err := s.Update(func(tx storage.Txn) error {
					_, err := CreateNode(tx, []string{"Person"}, map[string]any{"i": int64(i)})
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for g := 0; g < goroutines; g++ {
		if err := <-errs; err != nil {
			t.Fatalf("goroutine: %v", err)
		}
	}

	// Total node count and per-node index consistency.
	var ids []uint64
	mustView(t, s, func(tx storage.Txn) error {
		lid, ok, err := catalog.LookupLabel(tx, "Person")
		if err != nil || !ok {
			t.Fatalf("LookupLabel: ok=%v err=%v", ok, err)
		}
		it := tx.Scan(codec.LabelPrefix(lid))
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			_, nid, err := codec.ParseLabelKey(it.Key())
			if err != nil {
				return err
			}
			ids = append(ids, nid)
		}
		return nil
	})
	if want := goroutines * perGoroutine; len(ids) != want {
		t.Fatalf("expected %d Person nodes, got %d", want, len(ids))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	mustView(t, s, func(tx storage.Txn) error {
		for _, id := range ids {
			assertNodeIndexConsistency(t, tx, id)
		}
		return nil
	})
}
