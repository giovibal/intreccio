package badger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/overlay"
)

// loadPendingWrites bounds the concurrency of a restore Load.
const loadPendingWrites = 256

// maxRetries bounds the Update attempts on an SSI conflict.
const maxRetries = 100

// Backoff between conflict retries: exponential from retryBase, capped at
// retryCap, with jitter so contending writers do not retry in lockstep.
const (
	retryBase = 50 * time.Microsecond
	retryCap  = 10 * time.Millisecond
)

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

// Update runs fn with its writes buffered in an overlay and hands them to
// Badger only when fn returns nil. A Badger iterator opened in a read-write
// transaction copies and sorts all of that transaction's pending writes, so
// with writes passed straight through, every index seek of a statement that
// writes k keys cost O(k log k), making bulk loads quadratic (#65). Reads
// still go through the Badger transaction, so conflict detection is unchanged.
func (s *Store) Update(fn func(storage.Txn) error) error {
	for i := 0; ; i++ {
		err := s.db.Update(func(tx *badger.Txn) error {
			t := &txn{tx: tx, write: true, store: s}
			if err := fn(t); err != nil {
				return err
			}
			return t.flush()
		})
		if !errors.Is(err, badger.ErrConflict) {
			return err
		}
		if i >= maxRetries {
			return storage.ErrConflict
		}
		time.Sleep(retryDelay(i))
	}
}

// retryDelay is the jittered exponential backoff before conflict retry i:
// a random duration in [d/2, d) with d = min(retryBase*2^i, retryCap).
func retryDelay(i int) time.Duration {
	d := retryCap
	if i < 20 && retryBase<<i < retryCap {
		d = retryBase << i
	}
	return d/2 + rand.N(d/2)
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
	// For Update: the buffered writes and their estimated size, checked
	// against Badger's per-transaction limits as they grow.
	write       bool
	store       *Store
	ov          overlay.Tree
	count, size int64
	done        bool
}

var _ storage.Txn = (*txn)(nil)

func (t *txn) Get(key []byte) ([]byte, error) {
	if v, deleted, found := t.ov.Get(key); found {
		if deleted {
			return nil, storage.ErrNotFound
		}
		return bytes.Clone(v), nil
	}
	item, err := t.tx.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, storage.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item.ValueCopy(nil)
}

func (t *txn) Set(key, val []byte) error {
	if val == nil {
		val = []byte{} // present and empty, unlike a deletion marker
	}
	if err := t.buffer(key, val); err != nil {
		return err
	}
	t.ov.Put(key, val)
	return nil
}

func (t *txn) Delete(key []byte) error {
	if err := t.buffer(key, nil); err != nil {
		return err
	}
	t.ov.Delete(key)
	return nil
}

// buffer checks a write before it is buffered: the transaction must be
// writable and stay within the batch Badger will accept at commit, so an
// oversized write fails as early as it did when Badger saw every write.
func (t *txn) buffer(key, val []byte) error {
	switch {
	case !t.write || t.done:
		return badger.ErrReadOnlyTxn
	case len(key) == 0:
		return badger.ErrEmptyKey
	}
	old, oldDeleted, found := t.ov.Get(key)
	if found {
		t.size -= t.entrySize(key, old, oldDeleted)
	} else {
		t.count++
	}
	t.size += t.entrySize(key, val, val == nil)
	if t.count >= t.store.db.MaxBatchCount() || t.size >= t.store.db.MaxBatchSize() {
		return storage.ErrTxnTooLarge
	}
	return nil
}

// entrySize mirrors Badger's estimate for one write: key, value or value
// pointer, two meta bytes and the 8-byte version (rounded to 10).
func (t *txn) entrySize(key, val []byte, deleted bool) int64 {
	v := int64(len(val))
	if deleted {
		v = 0
	}
	if v >= t.store.db.Opts().ValueThreshold {
		v = 12
	}
	return int64(len(key)) + v + 2 + 10
}

// flush hands the buffered writes to Badger, in key order.
func (t *txn) flush() error {
	t.done = true
	for it := t.ov.Seek(nil); it.Valid(); it.Next() {
		v, deleted := it.Value()
		var err error
		if deleted {
			err = t.tx.Delete(it.Key())
		} else {
			err = t.tx.Set(it.Key(), v)
		}
		if err != nil {
			return mapWriteErr(err)
		}
	}
	return nil
}

// mapWriteErr turns Badger's per-transaction size limit into the
// engine-neutral storage.ErrTxnTooLarge.
func mapWriteErr(err error) error {
	if errors.Is(err, badger.ErrTxnTooBig) {
		return storage.ErrTxnTooLarge
	}
	return err
}

func (t *txn) Scan(prefix []byte) storage.Iterator {
	return t.ScanFrom(prefix, prefix)
}

// ScanFrom merges the committed keys with the writes buffered so far; writes
// made while the iterator is open stay invisible to it.
func (t *txn) ScanFrom(prefix, start []byte) storage.Iterator {
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	// Value prefetching starts one goroutine per key. Most scans here read
	// keys only (index and adjacency entries have empty values), and node
	// records are small enough to read on demand, so it costs more than it
	// saves.
	opts.PrefetchValues = false
	it := t.tx.NewIterator(opts)
	it.Seek(start)
	base := &iterator{it: it, prefix: prefix}
	if t.ov.Len() == 0 {
		return base
	}
	return overlay.NewMerged(source{base}, t.ov.Seek(start), prefix)
}

// source is the committed side of a merged scan: Key need not be copied,
// the merge copies the keys it hands out.
type source struct{ *iterator }

func (s source) Key() []byte { return s.it.Item().Key() }

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
