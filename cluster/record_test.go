package cluster

import (
	"errors"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
)

// TestRecordTxnStageReplay covers the recording transaction end to end:
// read-your-writes during staging, rollback of the staging txn, and
// order-preserving replay of the captured write-set into a fresh store.
func TestRecordTxnStageReplay(t *testing.T) {
	src, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatalf("src open: %v", err)
	}
	defer func() { _ = src.Close() }()

	var writeSet []byte
	err = src.Update(func(real storage.Txn) error {
		rec := newRecordTxn(real)
		if err := rec.Set([]byte("a"), []byte("1")); err != nil {
			return err
		}
		if err := rec.Set([]byte("b"), []byte("2")); err != nil {
			return err
		}
		// read-your-writes: the just-written value is visible within staging.
		if got, err := rec.Get([]byte("a")); err != nil || string(got) != "1" {
			t.Fatalf("read-your-writes Get(a) = %q, %v; want \"1\", nil", got, err)
		}
		if err := rec.Delete([]byte("a")); err != nil {
			return err
		}
		// a is now deleted within the same staging txn.
		if _, err := rec.Get([]byte("a")); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Get(a) after delete = %v; want ErrNotFound", err)
		}
		writeSet = rec.encode()
		return errStaged // roll back staging; effects are captured in writeSet
	})
	if err != nil && !errors.Is(err, errStaged) {
		t.Fatalf("stage: %v", err)
	}

	// Staging was rolled back: the source store is untouched.
	if n := keyCount(t, src); n != 0 {
		t.Fatalf("after staged rollback, src has %d keys; want 0", n)
	}

	// Replay the write-set into a fresh store; it must reflect the ordered
	// effects (set a, set b, delete a) → only b survives.
	dst, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatalf("dst open: %v", err)
	}
	defer func() { _ = dst.Close() }()
	if err := dst.Update(func(txn storage.Txn) error {
		return applyWriteSet(writeSet, txn)
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if err := dst.View(func(txn storage.Txn) error {
		if _, err := txn.Get([]byte("a")); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("replay Get(a) = %v; want ErrNotFound", err)
		}
		got, err := txn.Get([]byte("b"))
		if err != nil || string(got) != "2" {
			t.Errorf("replay Get(b) = %q, %v; want \"2\", nil", got, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func keyCount(t *testing.T, store *badgerstore.Store) int {
	t.Helper()
	var n int
	if err := store.View(func(txn storage.Txn) error {
		it := txn.Scan(nil)
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}
