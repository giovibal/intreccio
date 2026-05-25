// Package mycypher è l'API pubblica embeddable del graph DB.
package mycypher

// DB è l'handle del database. In Fase 1 incapsulerà lo storage.Store.
type DB struct{}

// Open apre (o crea) il database nella directory indicata.
func Open(path string) (*DB, error) {
	return &DB{}, nil
}

// Close rilascia le risorse del database.
func (db *DB) Close() error {
	return nil
}
