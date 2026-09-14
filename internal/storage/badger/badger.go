package badger

import (
	"errors"
	"fmt"
	"io"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/giovibal/intreccio/internal/storage"
)

// loadPendingWrites bounds the concurrency of a restore Load.
const loadPendingWrites = 256

// maxRetries bounds the Update attempts on an SSI conflict.
const maxRetries = 100

// Store is the storage.Store adapter over BadgerDB.
type Store struct {
	db *badger.DB
}

// Options tunes the Badger engine. The zero value is the durable default, so a
// plain Options{} fsyncs committed writes.
type Options struct {
	// SyncWrites makes every committed write durable (fsync) before Update
	// returns, at some throughput cost. When false, recent writes may be lost
	// after an OS crash or power loss; the store stays consistent either way.
	SyncWrites bool
}

var _ storage.Store = (*Store)(nil)

// Open opens (or creates) a durable Badger database in the given directory.
// It is equivalent to OpenWithOptions with the durable defaults.
func Open(path string) (*Store, error) {
	return OpenWithOptions(path, Options{SyncWrites: true})
}

// OpenWithOptions opens (or creates) a Badger database in the given directory
// with explicit engine options.
func OpenWithOptions(path string, opts Options) (*Store, error) {
	return open(badger.DefaultOptions(path).WithSyncWrites(opts.SyncWrites))
}

// OpenInMemory opens a fully in-RAM Badger database (useful in tests).
func OpenInMemory() (*Store, error) {
	return open(badger.DefaultOptions("").WithInMemory(true))
}

func open(opts badger.Options) (*Store, error) {
	opts.Logger = nil // silent
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("badger: open: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) View(fn func(storage.Txn) error) error {
	return s.db.View(func(tx *badger.Txn) error { return fn(&txn{tx: tx}) })
}

func (s *Store) Update(fn func(storage.Txn) error) error {
	for i := 0; ; i++ {
		err := s.db.Update(func(tx *badger.Txn) error { return fn(&txn{tx: tx}) })
		if errors.Is(err, badger.ErrConflict) && i < maxRetries {
			continue
		}
		return err
	}
}

func (s *Store) Close() error { return s.db.Close() }

var _ storage.Snapshotter = (*Store)(nil)

// Backup writes a consistent dump of the whole database as of now.
func (s *Store) Backup(w io.Writer) error {
	_, err := s.db.Backup(w, 0)
	if err != nil {
		return fmt.Errorf("badger: backup: %w", err)
	}
	return nil
}

// Load replaces the entire database contents with the dump from r. Existing
// data is dropped first so the result reflects exactly the snapshot.
//
// The replacement is not atomic: Badger has no single call that swaps the whole
// keyspace. If DropAll succeeds but Load fails (or the process dies in between),
// the store is left empty or partial and the caller must restore again before
// trusting it. The Raft layer is resilient to this because it re-applies the
// latest snapshot on every startup, so an interrupted restore is retried rather
// than silently served.
//
// Badger's Load trusts the length prefix of each record and panics on a corrupt
// payload (it calls make with an attacker/time-controlled size). Recover it and
// return an error so a damaged snapshot lets Raft fall back to an older snapshot
// or fail cleanly, instead of taking the process down.
func (s *Store) Load(r io.Reader) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("badger: load: corrupt backup: %v", p)
		}
	}()
	if err := s.db.DropAll(); err != nil {
		return fmt.Errorf("badger: load: drop: %w", err)
	}
	if err := s.db.Load(r, loadPendingWrites); err != nil {
		return fmt.Errorf("badger: load: %w", err)
	}
	return nil
}

type txn struct {
	tx *badger.Txn
}

var _ storage.Txn = (*txn)(nil)

func (t *txn) Get(key []byte) ([]byte, error) {
	item, err := t.tx.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

func (t *txn) Set(key, val []byte) error { return t.tx.Set(key, val) }

func (t *txn) Delete(key []byte) error { return t.tx.Delete(key) }

func (t *txn) Scan(prefix []byte) storage.Iterator {
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	it := t.tx.NewIterator(opts)
	it.Seek(prefix)
	return &iterator{it: it, prefix: prefix}
}

type iterator struct {
	it     *badger.Iterator
	prefix []byte
}

var _ storage.Iterator = (*iterator)(nil)

func (i *iterator) Valid() bool { return i.it.ValidForPrefix(i.prefix) }

func (i *iterator) Next() { i.it.Next() }

func (i *iterator) Key() []byte { return i.it.Item().KeyCopy(nil) }

func (i *iterator) Value() ([]byte, error) { return i.it.Item().ValueCopy(nil) }

func (i *iterator) Close() error {
	i.it.Close()
	return nil
}
