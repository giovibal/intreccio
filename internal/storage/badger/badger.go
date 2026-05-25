package badger

import (
	"errors"
	"fmt"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/giovibal/mycypher/internal/storage"
)

// maxRetries limita i tentativi di Update in caso di conflitto SSI.
const maxRetries = 100

// Store è l'adapter storage.Store su BadgerDB.
type Store struct {
	db *badger.DB
}

var _ storage.Store = (*Store)(nil)

// Open apre (o crea) un database Badger nella directory indicata.
func Open(path string) (*Store, error) {
	return open(badger.DefaultOptions(path))
}

// OpenInMemory apre un database Badger interamente in RAM (utile nei test).
func OpenInMemory() (*Store, error) {
	return open(badger.DefaultOptions("").WithInMemory(true))
}

func open(opts badger.Options) (*Store, error) {
	opts.Logger = nil // silenzioso
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
