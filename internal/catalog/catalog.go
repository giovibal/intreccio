// Package catalog manages name↔id dictionaries, ID counters and the index registry.
//
// Keyspace (first byte of the key), disjoint from the graph tags (n/e/o/i/l/p):
//
//	c + kind(1)                  -> uint64   counter (kind: n,e,L,T,K)
//	L + name                     -> id(4)    label dictionary (name→id)
//	T + name                     -> id(4)    type dictionary
//	K + name                     -> id(4)    property-key dictionary
//	R + kind(1) + id(4)          -> name     dictionary reverse
//	X + label(4) + propKey(4)    -> {}       registry of active `p` indexes
//
// IDs start at 1; ID 0 is reserved as "none".
package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

const (
	tagCounter  byte = 'c'
	tagLabelFwd byte = 'L'
	tagTypeFwd  byte = 'T'
	tagKeyFwd   byte = 'K'
	tagReverse  byte = 'R'
	tagIndexReg byte = 'X'
)

// Dictionary kinds, used for the counter and the reverse mapping.
const (
	kindLabel byte = 'L'
	kindType  byte = 'T'
	kindKey   byte = 'K'
	kindNode  byte = 'n'
	kindEdge  byte = 'e'
)

// InternLabel returns the label ID, allocating it if new (idempotent).
func InternLabel(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagLabelFwd, kindLabel, name)
}

// InternType returns the relationship type ID, allocating it if new.
func InternType(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagTypeFwd, kindType, name)
}

// InternKey returns the property-key ID, allocating it if new.
func InternKey(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagKeyFwd, kindKey, name)
}

// LookupLabel resolves a label name to its ID without allocating it. found is
// false if the label does not exist (useful in the read path inside a View).
func LookupLabel(txn storage.Txn, name string) (id uint32, found bool, err error) {
	return lookupDict(txn, tagLabelFwd, name)
}

// LookupType resolves a type name to its ID without allocating it.
func LookupType(txn storage.Txn, name string) (id uint32, found bool, err error) {
	return lookupDict(txn, tagTypeFwd, name)
}

// LookupKey resolves a property-key name to its ID without allocating it.
func LookupKey(txn storage.Txn, name string) (id uint32, found bool, err error) {
	return lookupDict(txn, tagKeyFwd, name)
}

func lookupDict(txn storage.Txn, fwdTag byte, name string) (uint32, bool, error) {
	fwd := append([]byte{fwdTag}, name...)
	switch v, err := txn.Get(fwd); {
	case err == nil:
		return binary.BigEndian.Uint32(v), true, nil
	case errors.Is(err, storage.ErrNotFound):
		return 0, false, nil
	default:
		return 0, false, err
	}
}

// LabelName resolves a label ID to its name.
func LabelName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindLabel, id) }

// TypeName resolves a type ID to its name.
func TypeName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindType, id) }

// KeyName resolves a property-key ID to its name.
func KeyName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindKey, id) }

// NextNodeID allocates a new monotonic node ID.
func NextNodeID(txn storage.Txn) (uint64, error) { return nextCounter(txn, kindNode) }

// NextEdgeID allocates a new monotonic edge ID.
func NextEdgeID(txn storage.Txn) (uint64, error) { return nextCounter(txn, kindEdge) }

func internDict(txn storage.Txn, fwdTag, kind byte, name string) (uint32, error) {
	fwd := append([]byte{fwdTag}, name...)
	switch v, err := txn.Get(fwd); {
	case err == nil:
		return binary.BigEndian.Uint32(v), nil
	case !errors.Is(err, storage.ErrNotFound):
		return 0, err
	}

	id64, err := nextCounter(txn, kind)
	if err != nil {
		return 0, err
	}
	if id64 > 0xFFFFFFFF {
		return 0, fmt.Errorf("catalog: ran out of IDs for dictionary %c", kind)
	}
	id := uint32(id64)

	var idb [4]byte
	binary.BigEndian.PutUint32(idb[:], id)
	if err := txn.Set(fwd, idb[:]); err != nil {
		return 0, err
	}
	rev := append([]byte{tagReverse, kind}, idb[:]...)
	if err := txn.Set(rev, []byte(name)); err != nil {
		return 0, err
	}
	return id, nil
}

func revName(txn storage.Txn, kind byte, id uint32) (string, error) {
	var idb [4]byte
	binary.BigEndian.PutUint32(idb[:], id)
	key := append([]byte{tagReverse, kind}, idb[:]...)
	v, err := txn.Get(key)
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// nextCounter increments the counter of the given kind and returns the new value
// (pre-increment: 1, 2, 3, ...).
func nextCounter(txn storage.Txn, kind byte) (uint64, error) {
	key := []byte{tagCounter, kind}
	var cur uint64
	switch v, err := txn.Get(key); {
	case err == nil:
		cur = binary.BigEndian.Uint64(v)
	case !errors.Is(err, storage.ErrNotFound):
		return 0, err
	}
	next := cur + 1
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], next)
	if err := txn.Set(key, b[:]); err != nil {
		return 0, err
	}
	return next, nil
}

// IndexDef identifies a secondary `p` index on (label, propKey).
type IndexDef struct {
	Label   uint32
	PropKey uint32
}

func indexKey(label, propKey uint32) []byte {
	b := make([]byte, 1+4+4)
	b[0] = tagIndexReg
	binary.BigEndian.PutUint32(b[1:5], label)
	binary.BigEndian.PutUint32(b[5:9], propKey)
	return b
}

// AddIndex registers an active `p` index (idempotent).
func AddIndex(txn storage.Txn, label, propKey uint32) error {
	return txn.Set(indexKey(label, propKey), []byte{})
}

// HasIndex reports whether a `p` index exists on (label, propKey).
func HasIndex(txn storage.Txn, label, propKey uint32) (bool, error) {
	switch _, err := txn.Get(indexKey(label, propKey)); {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// ListIndexes lists the registered `p` indexes.
func ListIndexes(txn storage.Txn) ([]IndexDef, error) {
	var out []IndexDef
	it := txn.Scan([]byte{tagIndexReg})
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 1+4+4 {
			return nil, fmt.Errorf("catalog: malformed index registry key (len=%d)", len(k))
		}
		out = append(out, IndexDef{
			Label:   binary.BigEndian.Uint32(k[1:5]),
			PropKey: binary.BigEndian.Uint32(k[5:9]),
		})
	}
	return out, nil
}
