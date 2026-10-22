// Package backend is the private seam between the root intreccio package and
// the optional cluster package: cluster implements Backend and turns it into
// an *intreccio.DB through NewDB, without either type being part of the public
// API.
package backend

import "github.com/giovibal/intreccio/internal/storage"

// Backend is the storage/execution backend behind a DB: the local engine, a
// cluster voter or a dataless cluster client.
type Backend interface {
	// View runs fn in a read-only transaction.
	View(fn func(storage.Txn) error) error
	// ApplyWrite executes a write via stage and durably commits/replicates its
	// effects, returning whatever stage produced.
	ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error)
	// Close releases the backend.
	Close() error
}

// NewDB wraps a Backend in an *intreccio.DB (returned as any: this package
// cannot import the root one). The root package sets it in its init, which
// always runs before any importer of the root package (such as cluster) can
// call it; nothing else may assign it.
var NewDB func(Backend) any
