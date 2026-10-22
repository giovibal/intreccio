// Package bolt is the storage.Store adapter over bbolt, an alternative to the
// default Badger engine.
//
// bbolt has one writer at a time, so Update never conflicts, and its cursors
// must not outlive a write to the same bucket. To keep the semantics the upper
// layer relies on (read-your-writes, iterators that stay valid while the
// transaction keeps writing), an Update buffers its writes in memory and
// applies them to the bucket only when fn returns nil. Reads merge the buffer
// with the committed data; an iterator sees the writes made before it was
// created, as with Badger.
package bolt

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/giovibal/intreccio/internal/storage"
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
		t := &txn{b: b, pending: map[string][]byte{}}
		if err := fn(t); err != nil {
			return err
		}
		t.closed = true
		keys := make([]string, 0, len(t.pending))
		for k := range t.pending {
			keys = append(keys, k)
		}
		slices.Sort(keys) // sequential inserts keep the B+tree compact
		for _, k := range keys {
			v := t.pending[k]
			var err error
			if v == nil {
				err = b.Delete([]byte(k))
			} else {
				err = b.Put([]byte(k), v)
			}
			if err != nil {
				return fmt.Errorf("bolt: commit: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) Close() error { return s.db.Close() }

// txn is a bbolt transaction plus, for Update, its buffered writes: a nil
// value in pending marks a deletion. Stored values are never nil (Set stores
// an empty slice for an empty value).
type txn struct {
	b       *bolt.Bucket
	pending map[string][]byte // nil map for a read-only transaction
	closed  bool
}

var _ storage.Txn = (*txn)(nil)

var errReadOnly = errors.New("bolt: write in a read-only transaction")

func (t *txn) Get(key []byte) ([]byte, error) {
	if v, ok := t.pending[string(key)]; ok {
		if v == nil {
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
	if t.pending == nil || t.closed {
		return errReadOnly
	}
	if len(key) == 0 {
		return errors.New("bolt: empty key")
	}
	v := make([]byte, len(val)) // non-nil even when empty
	copy(v, val)
	t.pending[string(key)] = v
	return nil
}

func (t *txn) Delete(key []byte) error {
	if t.pending == nil || t.closed {
		return errReadOnly
	}
	t.pending[string(key)] = nil
	return nil
}

func (t *txn) Scan(prefix []byte) storage.Iterator { return t.ScanFrom(prefix, prefix) }

func (t *txn) ScanFrom(prefix, start []byte) storage.Iterator {
	if bytes.Compare(start, prefix) < 0 {
		start = prefix
	}
	it := &iterator{prefix: prefix, cur: t.b.Cursor()}
	// Snapshot the buffered writes in range: later writes stay invisible, as
	// with a Badger iterator.
	for k, v := range t.pending {
		if bytes.HasPrefix([]byte(k), prefix) && k >= string(start) {
			it.pend = append(it.pend, pendingEntry{key: []byte(k), val: v})
		}
	}
	slices.SortFunc(it.pend, func(a, b pendingEntry) int { return bytes.Compare(a.key, b.key) })
	it.ck, it.cv = it.cur.Seek(start)
	it.settle()
	return it
}

type pendingEntry struct {
	key, val []byte // val nil = deleted
}

// iterator merges the committed keys (cursor) with the buffered writes
// (pend), the latter winning on equal keys and hiding deleted ones.
type iterator struct {
	prefix []byte
	cur    *bolt.Cursor
	ck, cv []byte // cursor position; ck nil when exhausted
	pend   []pendingEntry
	pi     int
	key    []byte // current merged entry; nil when done
	val    []byte
	closed bool
}

var _ storage.Iterator = (*iterator)(nil)

func (i *iterator) cursorKey() []byte {
	if i.ck == nil || !bytes.HasPrefix(i.ck, i.prefix) {
		return nil
	}
	return i.ck
}

// settle moves to the next visible entry, consuming the sources as needed.
func (i *iterator) settle() {
	for {
		ck := i.cursorKey()
		var pk []byte
		if i.pi < len(i.pend) {
			pk = i.pend[i.pi].key
		}
		switch {
		case ck == nil && pk == nil:
			i.key, i.val = nil, nil
			return
		case pk == nil || (ck != nil && bytes.Compare(ck, pk) < 0):
			i.key, i.val = ck, i.cv
			return
		default:
			p := i.pend[i.pi]
			if ck != nil && bytes.Equal(ck, pk) {
				i.ck, i.cv = i.cur.Next() // shadowed by the buffered write
			}
			if p.val == nil {
				i.pi++ // deleted in this transaction
				continue
			}
			i.key, i.val = p.key, p.val
			return
		}
	}
}

func (i *iterator) Valid() bool { return !i.closed && i.key != nil }

func (i *iterator) Next() {
	if !i.Valid() {
		return
	}
	if i.pi < len(i.pend) && bytes.Equal(i.key, i.pend[i.pi].key) {
		i.pi++
	} else {
		i.ck, i.cv = i.cur.Next()
	}
	i.settle()
}

func (i *iterator) Key() []byte { return bytes.Clone(i.key) }

func (i *iterator) Value() ([]byte, error) {
	v := make([]byte, len(i.val))
	copy(v, i.val)
	return v, nil
}

func (i *iterator) Close() error {
	i.closed = true
	return nil
}
