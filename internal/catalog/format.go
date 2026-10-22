package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

// tagFormat is the first byte of the on-disk format marker key. It is disjoint
// from the graph tags (n/e/o/i/l/p) and the other catalog tags (c/L/T/K/R/X/U).
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
	if v := binary.BigEndian.Uint32(cur); v != formatVersion && v != formatUnique {
		return fmt.Errorf("%w: database is version %d, this build supports %d and %d",
			ErrFormatVersion, v, formatVersion, formatUnique)
	}
	return nil
}

// RequireUniqueFormat raises the format marker to formatUnique (idempotent).
// It runs in the transaction that creates a unique constraint.
func RequireUniqueFormat(txn storage.Txn) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], formatUnique)
	return txn.Set(formatKey, b[:])
}
