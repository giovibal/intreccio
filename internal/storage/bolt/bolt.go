// Package bolt is the storage.Store adapter over bbolt, an alternative to the
// default Badger engine.
//
// bbolt has one writer at a time, so Update never conflicts, and its cursors
// must not outlive a write to the same bucket. To keep the semantics the upper
// layer relies on (read-your-writes, iterators that stay valid while the
// transaction keeps writing), an Update buffers its writes in memory (package
// overlay, shared with the Badger adapter) and applies them to the bucket
// only when fn returns nil. Reads merge the buffer
// with the committed data; an iterator sees the writes made before it was
// created, as with Badger.
package bolt

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/overlay"
)

// bucket holds the whole keyspace; key tags already separate the namespaces.
var bucket = []byte("intreccio")

// Options tunes the bbolt engine. The zero value is the durable default.
type Options struct {
	// NoSync skips the fsync on commit: faster writes, but recent ones may be
	// lost after an OS crash or power loss.
	NoSync bool
}

// Store is the storage.Store adapter over bbolt.
type Store struct {
	db *bolt.DB
}

var _ storage.Store = (*Store)(nil)

// Open opens (or creates) a durable bbolt database file at path.
func Open(path string) (*Store, error) { return OpenWithOptions(path, Options{}) }

// OpenWithOptions opens (or creates) a bbolt database file with explicit options.
func OpenWithOptions(path string, opts Options) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{
		Timeout:      time.Second, // fail instead of waiting forever on a locked file
		NoSync:       opts.NoSync,
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("bolt: open: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("bolt: open: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) View(fn func(storage.Txn) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return fn(&txn{b: tx.Bucket(bucket)})
	})
}

// Update runs fn with buffered writes and commits them atomically when fn
// returns nil. It never returns storage.ErrConflict: bbolt serializes writers.
func (s *Store) Update(fn func(storage.Txn) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		t := &txn{b: b, write: true}
		if err := fn(t); err != nil {
			return err
		}
		t.closed = true
		// In key order: sequential inserts keep the B+tree compact.
		for it := t.ov.Seek(nil); it.Valid(); it.Next() {
			v, deleted := it.Value()
			var err error
			if deleted {
				err = b.Delete(it.Key())
			} else {
				err = b.Put(it.Key(), v)
			}
			if err != nil {
				return fmt.Errorf("bolt: commit: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) Close() error { return s.db.Close() }

// txn is a bbolt transaction plus, for Update, its buffered writes.
type txn struct {
	b      *bolt.Bucket
	write  bool
	ov     overlay.Tree
	closed bool
}

var _ storage.Txn = (*txn)(nil)

var errReadOnly = errors.New("bolt: write in a read-only transaction")

func (t *txn) Get(key []byte) ([]byte, error) {
	if v, deleted, found := t.ov.Get(key); found {
		if deleted {
			return nil, storage.ErrNotFound
		}
		return bytes.Clone(v), nil
	}
	v := t.b.Get(key)
	if v == nil {
		return nil, storage.ErrNotFound
	}
	return bytes.Clone(v), nil
}

func (t *txn) Set(key, val []byte) error {
	if !t.write || t.closed {
		return errReadOnly
	}
	if len(key) == 0 {
		return errors.New("bolt: empty key")
	}
	if val == nil {
		val = []byte{} // present and empty, unlike a deletion marker
	}
	t.ov.Put(key, val)
	return nil
}

func (t *txn) Delete(key []byte) error {
	if !t.write || t.closed {
		return errReadOnly
	}
	t.ov.Delete(key)
	return nil
}

func (t *txn) Scan(prefix []byte) storage.Iterator { return t.ScanFrom(prefix, prefix) }

// ScanFrom merges the committed keys with the writes buffered so far; writes
// made while the iterator is open stay invisible to it, as with Badger.
func (t *txn) ScanFrom(prefix, start []byte) storage.Iterator {
	if bytes.Compare(start, prefix) < 0 {
		start = prefix
	}
	c := &cursor{prefix: prefix, cur: t.b.Cursor()}
	c.k, c.v = c.cur.Seek(start)
	return overlay.NewMerged(c, t.ov.Seek(start), prefix)
}

// cursor is the committed side of a scan: a bbolt cursor bounded by prefix.
type cursor struct {
	prefix []byte
	cur    *bolt.Cursor
	k, v   []byte // nil k when exhausted
}

func (c *cursor) Valid() bool { return c.k != nil && bytes.HasPrefix(c.k, c.prefix) }

func (c *cursor) Key() []byte { return c.k }

func (c *cursor) Value() ([]byte, error) {
	out := make([]byte, len(c.v)) // non-nil even when empty
	copy(out, c.v)
	return out, nil
}

func (c *cursor) Next() { c.k, c.v = c.cur.Next() }

func (c *cursor) Close() error {
	c.k = nil
	return nil
}
