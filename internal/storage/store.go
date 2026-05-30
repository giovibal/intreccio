package storage

import (
	"errors"
	"io"
)

// ErrNotFound is returned by Txn.Get when the key does not exist.
var ErrNotFound = errors.New("storage: key not found")

// Snapshotter is an optional capability for engines that can serialize and
// restore their full contents. It is used by the clustering layer for Raft
// snapshot/restore. Backup writes a consistent dump as of the call; Load
// replaces the entire contents with the dump. Not every Store implements it.
type Snapshotter interface {
	Backup(w io.Writer) error
	Load(r io.Reader) error
}

// Store is the minimal abstraction over the ordered KV engine. The upper layer
// does not depend on the concrete engine (DESIGN §3).
type Store interface {
	// View runs fn in a read-only transaction (consistent snapshot).
	View(fn func(Txn) error) error
	// Update runs fn in an atomic read-write transaction. The implementation may
	// retry fn on serialization conflict, so fn must be free of side effects
	// outside the transaction.
	Update(fn func(Txn) error) error
	// Close closes the store.
	Close() error
}

// Txn is a transaction. Keys are ordered lexicographically.
type Txn interface {
	// Get returns the value for the key, or ErrNotFound if absent.
	// The returned buffer is owned by the caller.
	Get(key []byte) ([]byte, error)
	Set(key, val []byte) error
	Delete(key []byte) error
	// Scan returns an iterator over the keys with the given prefix, in ascending
	// order. The caller must close the iterator.
	Scan(prefix []byte) Iterator
}

// Iterator walks a range of keys. Typical use:
//
//	it := txn.Scan(prefix)
//	defer it.Close()
//	for ; it.Valid(); it.Next() { ... }
type Iterator interface {
	Valid() bool
	Next()
	// Key returns the current key; the buffer is owned by the caller.
	Key() []byte
	// Value returns the current value; the buffer is owned by the caller.
	Value() ([]byte, error)
	Close() error
}
