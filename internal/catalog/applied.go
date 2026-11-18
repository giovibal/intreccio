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

// tagStaleChecks is the first byte of the key that turns on the stale-write
// check of a cluster (ADR 0007). Disjoint from every other tag.
const tagStaleChecks byte = 'S'

var staleChecksKey = []byte{tagStaleChecks}

// EnableStaleChecks records that every member of the cluster checks the base
// of a write-set entry, so that from now on every entry must carry one. It is
// written by a replicated write-set, like the data, and never removed.
func EnableStaleChecks(txn storage.Txn) error {
	return txn.Set(staleChecksKey, []byte{1})
}

// StaleChecks reports whether EnableStaleChecks was written in the state txn
// reads.
func StaleChecks(txn storage.Txn) (bool, error) {
	_, err := txn.Get(staleChecksKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}
