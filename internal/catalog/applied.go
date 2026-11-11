package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

// tagApplied is the first byte of the applied-index key. It is disjoint from
// the graph tags (n/e/o/i/l/p) and the other catalog tags (c/L/T/K/R/X/F).
const tagApplied byte = 'A'

var appliedKey = []byte{tagApplied}

// SetAppliedIndex records the Raft log index of the write being applied. A
// clustered voter calls it in the same transaction that applies the write-set,
// so the index and the data it describes can never disagree: any read
// transaction sees exactly the state as of the index it reads.
//
// It is node-local bookkeeping, not part of the replicated write-set, and an
// embedded database never writes it.
func SetAppliedIndex(txn storage.Txn, index uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], index)
	return txn.Set(appliedKey, b[:])
}

// AppliedIndex returns the Raft log index of the last write reflected in the
// state txn reads, or 0 when none was recorded (an embedded database, or a
// voter that has applied nothing yet).
func AppliedIndex(txn storage.Txn) (uint64, error) {
	v, err := txn.Get(appliedKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return 0, nil
	case err != nil:
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("catalog: malformed applied index (len=%d)", len(v))
	}
	return binary.BigEndian.Uint64(v), nil
}
