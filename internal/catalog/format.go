package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

// tagFormat is the first byte of the on-disk format marker key. It is disjoint
// from the graph tags (n/e/o/i/l/p/u/r) and the other catalog tags (c/L/T/K/R/X/U/Y).
const tagFormat byte = 'F'

// formatVersion is the on-disk layout version a new database starts with. Bump
// it whenever the key or value encoding changes incompatibly, so a binary
// refuses to open a database written in a different layout instead of
// silently misreading it.
const formatVersion uint32 = 1

// formatUnique is the version of a database that holds unique constraints
// (the `u` keys). It is raised by RequireUniqueFormat when the first
// constraint is created, so builds that do not maintain `u` keys refuse to
// open it instead of letting duplicates in. Databases without constraints
// keep formatVersion and stay readable by older builds.
const formatUnique uint32 = 2

// formatRelIndex is the version of a database that holds relationship
// property indexes (the `r` keys), raised the same way by the first one.
const formatRelIndex uint32 = 3

// maxFormat is the newest version this build reads.
const maxFormat = formatRelIndex

// ErrFormatVersion is returned when a database was written with an incompatible
// on-disk format version.
var ErrFormatVersion = errors.New("catalog: incompatible on-disk format version")

// formatKey is the single marker key.
var formatKey = []byte{tagFormat}

// EnsureFormat initialises the format marker on first use and verifies it on
// subsequent opens. It must run in a write transaction. The version is DB-wide
// metadata, not graph data: it is written once and never updated in place.
func EnsureFormat(txn storage.Txn) error {
	cur, err := txn.Get(formatKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], formatVersion)
		return txn.Set(formatKey, b[:])
	case err != nil:
		return err
	}
	if len(cur) != 4 {
		return fmt.Errorf("catalog: malformed format marker (len=%d)", len(cur))
	}
	if v := binary.BigEndian.Uint32(cur); v < formatVersion || v > maxFormat {
		return fmt.Errorf("%w: database is version %d, this build supports %d to %d",
			ErrFormatVersion, v, formatVersion, maxFormat)
	}
	return nil
}

// RequireUniqueFormat raises the format marker to at least formatUnique. It
// runs in the transaction that creates a unique constraint.
func RequireUniqueFormat(txn storage.Txn) error { return requireFormat(txn, formatUnique) }

// RequireRelIndexFormat raises the format marker to at least formatRelIndex.
// It runs in the transaction that creates a relationship index.
func RequireRelIndexFormat(txn storage.Txn) error { return requireFormat(txn, formatRelIndex) }

// requireFormat raises the marker to v unless it is already at least v: each
// version adds keys that older builds would not maintain.
func requireFormat(txn storage.Txn, v uint32) error {
	cur, err := txn.Get(formatKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if len(cur) == 4 && binary.BigEndian.Uint32(cur) >= v {
		return nil
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return txn.Set(formatKey, b[:])
}
