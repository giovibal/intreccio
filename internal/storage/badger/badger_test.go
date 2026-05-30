package badger

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSetGetDelete(t *testing.T) {
	s := newStore(t)
	key, val := []byte("k"), []byte("v")

	if err := s.Update(func(tx storage.Txn) error { return tx.Set(key, val) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(tx storage.Txn) error {
		got, err := tx.Get(key)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, val) {
			t.Errorf("got %q want %q", got, val)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(func(tx storage.Txn) error { return tx.Delete(key) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(tx storage.Txn) error {
		_, err := tx.Get(key)
		if !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("after delete expected ErrNotFound, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newStore(t)
	err := s.View(func(tx storage.Txn) error {
		_, err := tx.Get([]byte("missing"))
		return err
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestScanOrderAndPrefix(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		for _, k := range []string{"a:3", "a:1", "a:2", "b:1"} {
			if err := tx.Set([]byte(k), []byte("x")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := s.View(func(tx storage.Txn) error {
		it := tx.Scan([]byte("a:"))
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			got = append(got, string(it.Key()))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a:1", "a:2", "a:3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("scan got %v want %v", got, want)
	}
}

// Update must retry on an SSI conflict: N goroutines incrementing the same
// counter must converge to N without losing updates.
func TestUpdateRetriesOnConflict(t *testing.T) {
	s := newStore(t)
	key := []byte("counter")
	const n = 20

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(func(tx storage.Txn) error {
				var cur int
				if v, err := tx.Get(key); err == nil {
					_, _ = fmt.Sscanf(string(v), "%d", &cur)
				} else if !errors.Is(err, storage.ErrNotFound) {
					return err
				}
				return tx.Set(key, []byte(fmt.Sprintf("%d", cur+1)))
			})
			if err != nil {
				t.Errorf("Update: %v", err)
			}
		}()
	}
	wg.Wait()

	if err := s.View(func(tx storage.Txn) error {
		v, err := tx.Get(key)
		if err != nil {
			return err
		}
		if string(v) != fmt.Sprintf("%d", n) {
			t.Errorf("counter = %s, expected %d", v, n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
