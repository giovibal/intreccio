package storage

import "errors"

// ErrNotFound è restituito da Txn.Get quando la chiave non esiste.
var ErrNotFound = errors.New("storage: chiave non trovata")

// Store è l'astrazione minimale sull'engine KV ordinato. Lo strato superiore
// non dipende dall'engine concreto (DESIGN §3).
type Store interface {
	// View esegue fn in una transazione di sola lettura (snapshot coerente).
	View(fn func(Txn) error) error
	// Update esegue fn in una transazione read-write atomica. L'implementazione
	// può ritentare fn in caso di conflitto di serializzazione, quindi fn deve
	// essere priva di effetti collaterali esterni alla transazione.
	Update(fn func(Txn) error) error
	// Close chiude lo store.
	Close() error
}

// Txn è una transazione. Le chiavi sono ordinate lessicograficamente.
type Txn interface {
	// Get restituisce il valore della chiave, o ErrNotFound se assente.
	// Il buffer restituito è di proprietà del chiamante.
	Get(key []byte) ([]byte, error)
	Set(key, val []byte) error
	Delete(key []byte) error
	// Scan restituisce un iteratore sulle chiavi con il prefisso dato, in ordine
	// crescente. Il chiamante deve chiudere l'iteratore.
	Scan(prefix []byte) Iterator
}

// Iterator scorre un range di chiavi. Uso tipico:
//
//	it := txn.Scan(prefix)
//	defer it.Close()
//	for ; it.Valid(); it.Next() { ... }
type Iterator interface {
	Valid() bool
	Next()
	// Key restituisce la chiave corrente; il buffer è di proprietà del chiamante.
	Key() []byte
	// Value restituisce il valore corrente; il buffer è di proprietà del chiamante.
	Value() ([]byte, error)
	Close() error
}
