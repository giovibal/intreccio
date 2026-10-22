package graph

import (
	"bytes"
	"testing"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// DropIndexEntries removes every entry of one index and leaves the others
// and the node records alone (#35).
func TestDropIndexEntries(t *testing.T) {
	s := newStore(t)
	var lid, xid, yid uint32
	var ids []uint64
	mustUpdate(t, s, func(tx storage.Txn) error {
		var err error
		if lid, err = catalog.InternLabel(tx, "P"); err != nil {
			return err
		}
		if xid, err = catalog.InternKey(tx, "x"); err != nil {
			return err
		}
		if yid, err = catalog.InternKey(tx, "y"); err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, xid); err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, lid, yid); err != nil {
			return err
		}
		for i := int64(0); i < 50; i++ {
			id, err := CreateNode(tx, []string{"P"}, map[string]any{"x": i, "y": "v"})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	mustUpdate(t, s, func(tx storage.Txn) error { return DropIndexEntries(tx, lid, xid) })
	mustView(t, s, func(tx storage.Txn) error {
		count := func(prefix []byte) int {
			n := 0
			it := tx.Scan(prefix)
			defer func() { _ = it.Close() }()
			for ; it.Valid(); it.Next() {
				if !bytes.HasPrefix(it.Key(), prefix) {
					t.Fatal("scan left its prefix")
				}
				n++
			}
			return n
		}
		if n := count(codec.PropIndexPrefix(lid, xid)); n != 0 {
			t.Errorf("%d entries left in the dropped index", n)
		}
		if n := count(codec.PropIndexPrefix(lid, yid)); n != 50 {
			t.Errorf("the other index has %d entries, want 50", n)
		}
		if has, _ := catalog.HasIndex(tx, lid, xid); has {
			t.Error("the dropped index is still registered")
		}
		for _, id := range ids {
			assertNodeIndexConsistency(t, tx, id)
		}
		return nil
	})
}
