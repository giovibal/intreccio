package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

// tagFormat is the first byte of the on-disk format marker key. It is disjoint
// from the graph tags (n/e/o/i/l/p) and the other catalog tags (c/L/T/K/R/X).
const tagFormat byte = 'F'

// formatVersion is the current on-disk layout version. Bump it whenever the key
// or value encoding changes incompatibly, so a binary refuses to open a database
// written in a different layout instead of silently misreading it.
const formatVersion uint32 = 1

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
	if v := binary.BigEndian.Uint32(cur); v != formatVersion {
		return fmt.Errorf("%w: database is version %d, this build supports %d",
			ErrFormatVersion, v, formatVersion)
	}
	return nil
}
