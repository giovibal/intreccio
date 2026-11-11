// Package storagetest is a conformance suite for storage.Store engines: the
// behavior the upper layers rely on, checked the same way on every adapter.
package storagetest

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
)

// Opener opens (or reopens) a store whose data lives in dir.
type Opener func(t *testing.T, dir string) storage.Store

// Run runs the whole suite against the engine opened by open.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s storage.Store)
	}{
		{"SetGetDelete", testSetGetDelete},
		{"Rollback", testRollback},
		{"ReadYourWrites", testReadYourWrites},
		{"ScanOrderAndBounds", testScanOrderAndBounds},
		{"ScanFrom", testScanFrom},
		{"IteratorSnapshot", testIteratorSnapshot},
		{"WriteWhileIterating", testWriteWhileIterating},
		{"BuffersOwnedByCaller", testBuffersOwnedByCaller},
		{"WriteInView", testWriteInView},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, t.TempDir())
			defer func() { _ = s.Close() }()
			tc.fn(t, s)
		})
	}
	t.Run("Reopen", func(t *testing.T) { testReopen(t, open) })
}

func update(t *testing.T, s storage.Store, fn func(storage.Txn) error) {
	t.Helper()
	if err := s.Update(fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func get(t *testing.T, s storage.Store, key string) (string, bool) {
	t.Helper()
	var (
		val   []byte
		found bool
	)
	err := s.View(func(tx storage.Txn) error {
		v, err := tx.Get([]byte(key))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		val, found = v, err == nil
		return err
	})
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	return string(val), found
}

// keys lists the keys under prefix, starting at start, as "k=v" strings.
func keys(t *testing.T, tx storage.Txn, prefix, start string) []string {
	t.Helper()
	it := tx.ScanFrom([]byte(prefix), []byte(start))
	defer func() { _ = it.Close() }()
	var out []string
	for ; it.Valid(); it.Next() {
		v, err := it.Value()
		if err != nil {
			t.Fatalf("Value: %v", err)
		}
		out = append(out, string(it.Key())+"="+string(v))
	}
	return out
}

func viewKeys(t *testing.T, s storage.Store, prefix, start string) []string {
	t.Helper()
	var out []string
	if err := s.View(func(tx storage.Txn) error { out = keys(t, tx, prefix, start); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func want(t *testing.T, what string, got []string, exp ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(exp) {
		t.Errorf("%s = %v, want %v", what, got, exp)
	}
}

func testSetGetDelete(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		if err := tx.Set([]byte("a"), []byte("1")); err != nil {
			return err
		}
		return tx.Set([]byte("empty"), nil)
	})
	if v, ok := get(t, s, "a"); !ok || v != "1" {
		t.Errorf("a = %q, %v", v, ok)
	}
	if v, ok := get(t, s, "empty"); !ok || v != "" {
		t.Errorf("empty value = %q, %v; want present and empty", v, ok)
	}
	if _, ok := get(t, s, "missing"); ok {
		t.Error("missing key found")
	}
	update(t, s, func(tx storage.Txn) error { return tx.Delete([]byte("a")) })
	if _, ok := get(t, s, "a"); ok {
		t.Error("deleted key still found")
	}
	update(t, s, func(tx storage.Txn) error { return tx.Delete([]byte("never-set")) })
}

func testRollback(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error { return tx.Set([]byte("k"), []byte("old")) })
	boom := errors.New("boom")
	err := s.Update(func(tx storage.Txn) error {
		_ = tx.Set([]byte("k"), []byte("new"))
		_ = tx.Set([]byte("k2"), []byte("x"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	if v, _ := get(t, s, "k"); v != "old" {
		t.Errorf("k = %q after rollback, want old", v)
	}
	if _, ok := get(t, s, "k2"); ok {
		t.Error("k2 written despite rollback")
	}
}

func testReadYourWrites(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		for _, k := range []string{"p1", "p2", "p3"} {
			if err := tx.Set([]byte(k), []byte("c")); err != nil {
				return err
			}
		}
		return nil
	})
	update(t, s, func(tx storage.Txn) error {
		_ = tx.Set([]byte("p2"), []byte("u")) // overwrite committed
		_ = tx.Delete([]byte("p3"))           // delete committed
		_ = tx.Set([]byte("p0"), []byte("n")) // new, before the others
		_ = tx.Set([]byte("p4"), []byte("n")) // new, after the others
		_ = tx.Set([]byte("p5"), []byte("n"))
		_ = tx.Delete([]byte("p5")) // new then deleted
		if v, err := tx.Get([]byte("p2")); err != nil || string(v) != "u" {
			t.Errorf("Get p2 in txn = %q, %v", v, err)
		}
		if _, err := tx.Get([]byte("p3")); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("Get deleted p3 in txn = %v, want ErrNotFound", err)
		}
		want(t, "scan in txn", keys(t, tx, "p", "p"), "p0=n", "p1=c", "p2=u", "p4=n")
		return nil
	})
	want(t, "scan after commit", viewKeys(t, s, "p", "p"), "p0=n", "p1=c", "p2=u", "p4=n")
}

func testScanOrderAndBounds(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		for _, k := range [][]byte{{'a'}, {'b', 0xff}, {'b', 0x00}, {'b', 0x10}, {'b'}, {'c'}} {
			if err := tx.Set(k, []byte("v")); err != nil {
				return err
			}
		}
		return nil
	})
	var got [][]byte
	_ = s.View(func(tx storage.Txn) error {
		it := tx.Scan([]byte("b"))
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			got = append(got, it.Key())
		}
		return nil
	})
	exp := [][]byte{{'b'}, {'b', 0x00}, {'b', 0x10}, {'b', 0xff}}
	if len(got) != len(exp) {
		t.Fatalf("scan b = %x, want %x", got, exp)
	}
	for i := range exp {
		if !bytes.Equal(got[i], exp[i]) {
			t.Fatalf("scan b = %x, want %x", got, exp)
		}
	}
	want(t, "scan of a missing prefix", viewKeys(t, s, "z", "z"))
}

func testScanFrom(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		for _, k := range []string{"r1", "r3", "r5", "s1"} {
			if err := tx.Set([]byte(k), []byte("c")); err != nil {
				return err
			}
		}
		return nil
	})
	want(t, "from an existing key", viewKeys(t, s, "r", "r3"), "r3=c", "r5=c")
	want(t, "from between keys", viewKeys(t, s, "r", "r4"), "r5=c")
	want(t, "from past the prefix", viewKeys(t, s, "r", "r9"))
	update(t, s, func(tx storage.Txn) error {
		_ = tx.Set([]byte("r2"), []byte("n"))
		_ = tx.Set([]byte("r4"), []byte("n"))
		_ = tx.Delete([]byte("r5"))
		want(t, "ScanFrom in txn", keys(t, tx, "r", "r3"), "r3=c", "r4=n")
		return nil
	})
}

// testIteratorSnapshot: an iterator sees the writes made before it was
// created, not those made while it is open. Without this, a statement that
// creates nodes while scanning their label could loop forever.
func testIteratorSnapshot(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		_ = tx.Set([]byte("n1"), []byte("c"))
		_ = tx.Set([]byte("n2"), []byte("c"))
		return nil
	})
	update(t, s, func(tx storage.Txn) error {
		_ = tx.Set([]byte("n3"), []byte("before"))
		it := tx.Scan([]byte("n"))
		defer func() { _ = it.Close() }()
		var got []string
		for ; it.Valid(); it.Next() {
			got = append(got, string(it.Key()))
			if err := tx.Set([]byte("n9"), []byte("during")); err != nil {
				return err
			}
		}
		want(t, "keys seen", got, "n1", "n2", "n3")
		return nil
	})
	want(t, "after commit", viewKeys(t, s, "n", "n"), "n1=c", "n2=c", "n3=before", "n9=during")
}

// testWriteWhileIterating: deleting and rewriting keys of the scanned range
// keeps the iterator valid, and the writes are committed.
func testWriteWhileIterating(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error {
		for i := range 50 {
			if err := tx.Set(fmt.Appendf(nil, "w%02d", i), []byte("c")); err != nil {
				return err
			}
		}
		return nil
	})
	update(t, s, func(tx storage.Txn) error {
		it := tx.Scan([]byte("w"))
		defer func() { _ = it.Close() }()
		n := 0
		for ; it.Valid(); it.Next() {
			k := it.Key()
			if n%2 == 0 {
				if err := tx.Delete(k); err != nil {
					return err
				}
			} else if err := tx.Set(k, []byte("u")); err != nil {
				return err
			}
			n++
		}
		if n != 50 {
			t.Errorf("iterated %d keys, want 50", n)
		}
		return nil
	})
	got := viewKeys(t, s, "w", "w")
	if len(got) != 25 || got[0] != "w01=u" || got[24] != "w49=u" {
		t.Errorf("after commit = %v, want the 25 odd keys updated", got)
	}
}

// testBuffersOwnedByCaller: buffers returned by Get, Key and Value belong to
// the caller. (The reverse is not required: Set may keep its arguments.)
func testBuffersOwnedByCaller(t *testing.T, s storage.Store) {
	update(t, s, func(tx storage.Txn) error { return tx.Set([]byte("own"), []byte("abc")) })
	_ = s.View(func(tx storage.Txn) error {
		v, _ := tx.Get([]byte("own"))
		v[0] = 'Y'
		it := tx.Scan([]byte("own"))
		defer func() { _ = it.Close() }()
		k := it.Key()
		k[0] = 'Y'
		iv, _ := it.Value()
		iv[0] = 'Y'
		return nil
	})
	if v, ok := get(t, s, "own"); !ok || v != "abc" {
		t.Errorf("own = %q, %v after mutating returned buffers", v, ok)
	}
}

func testWriteInView(t *testing.T, s storage.Store) {
	err := s.View(func(tx storage.Txn) error { return tx.Set([]byte("k"), []byte("v")) })
	if err == nil {
		t.Error("Set in View succeeded")
	}
	if _, ok := get(t, s, "k"); ok {
		t.Error("write in View is visible")
	}
}

func testReopen(t *testing.T, open Opener) {
	dir := t.TempDir()
	s := open(t, dir)
	update(t, s, func(tx storage.Txn) error { return tx.Set([]byte("durable"), []byte("yes")) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	defer func() { _ = s.Close() }()
	if v, ok := get(t, s, "durable"); !ok || v != "yes" {
		t.Errorf("after reopen durable = %q, %v", v, ok)
	}
}
