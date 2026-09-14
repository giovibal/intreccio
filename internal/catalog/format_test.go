package catalog

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/giovibal/intreccio/internal/storage"
)

func TestEnsureFormatInitializesAndIsIdempotent(t *testing.T) {
	s := newStore(t)

	for i := 0; i < 2; i++ {
		if err := s.Update(EnsureFormat); err != nil {
			t.Fatalf("EnsureFormat run %d: %v", i, err)
		}
	}

	if err := s.View(func(tx storage.Txn) error {
		v, err := tx.Get(formatKey)
		if err != nil {
			return err
		}
		if len(v) != 4 {
			t.Fatalf("marker length = %d, want 4", len(v))
		}
		if got := binary.BigEndian.Uint32(v); got != formatVersion {
			t.Fatalf("marker = %d, want %d", got, formatVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureFormatRejectsWrongVersion(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], formatVersion+1)
		return tx.Set(formatKey, b[:])
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(EnsureFormat); !errors.Is(err, ErrFormatVersion) {
		t.Fatalf("err = %v, want ErrFormatVersion", err)
	}
}

func TestEnsureFormatRejectsMalformedMarker(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		return tx.Set(formatKey, []byte{1, 2})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(EnsureFormat); err == nil {
		t.Fatal("expected an error for a malformed format marker")
	}
}
