// Package mycypher is the public embeddable API of the graph DB.
package mycypher

// DB is the database handle. In Phase 1 it will wrap the storage.Store.
type DB struct{}

// Open opens (or creates) the database in the given directory.
func Open(path string) (*DB, error) {
	return &DB{}, nil
}

// Close releases the database resources.
func (db *DB) Close() error {
	return nil
}
