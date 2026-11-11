package catalog

import (
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
)

func TestAppliedIndex(t *testing.T) {
	s, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	read := func() uint64 {
		t.Helper()
		var idx uint64
		if err := s.View(func(txn storage.Txn) error {
			var err error
			idx, err = AppliedIndex(txn)
			return err
		}); err != nil {
			t.Fatalf("AppliedIndex: %v", err)
		}
		return idx
	}
	if got := read(); got != 0 {
		t.Errorf("fresh store: applied index = %d, want 0", got)
	}
	for _, want := range []uint64{1, 42, 1 << 40} {
		if err := s.Update(func(txn storage.Txn) error { return SetAppliedIndex(txn, want) }); err != nil {
			t.Fatalf("SetAppliedIndex: %v", err)
		}
		if got := read(); got != want {
			t.Errorf("applied index = %d, want %d", got, want)
		}
	}

	if err := s.Update(func(txn storage.Txn) error { return txn.Set(appliedKey, []byte{1, 2}) }); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(txn storage.Txn) error {
		_, err := AppliedIndex(txn)
		return err
	}); err == nil {
		t.Error("malformed applied index: want an error")
	}
}
