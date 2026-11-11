package badger

import (
	"errors"
	"testing"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/giovibal/intreccio/internal/storage"
)

func TestRetryDelayBounds(t *testing.T) {
	for i := 0; i <= maxRetries; i++ {
		d := retryCap
		if i < 20 && retryBase<<i < retryCap {
			d = retryBase << i
		}
		for range 20 {
			got := retryDelay(i)
			if got < d/2 || got >= d {
				t.Fatalf("retryDelay(%d) = %v, want in [%v, %v)", i, got, d/2, d)
			}
		}
	}
}

// TestUpdateReturnsErrConflict: a transaction that conflicts on every
// attempt ends with storage.ErrConflict after the retries, and commits
// nothing.
func TestUpdateReturnsErrConflict(t *testing.T) {
	s := newStore(t)
	key := []byte("k")
	attempts := 0
	start := time.Now()
	err := s.Update(func(tx storage.Txn) error {
		attempts++
		if _, err := tx.Get(key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		// A concurrent writer commits the key this transaction just read,
		// so its own commit always conflicts.
		if err := s.db.Update(func(btx *badger.Txn) error { return btx.Set(key, []byte("other")) }); err != nil {
			return err
		}
		return tx.Set([]byte("mine"), []byte("v"))
	})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("got err %v, want storage.ErrConflict", err)
	}
	if attempts != maxRetries+1 {
		t.Errorf("attempts = %d, want %d", attempts, maxRetries+1)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("retries took %v, backoff is too long", elapsed)
	}
	if err := s.View(func(tx storage.Txn) error {
		if _, err := tx.Get([]byte("mine")); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("a conflicting transaction must not commit, Get: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
