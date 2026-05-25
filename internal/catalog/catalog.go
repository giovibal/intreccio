// Package catalog gestisce dizionari name↔id, contatori ID e registry indici.
//
// Keyspace (primo byte della chiave), disgiunti dai tag del grafo (n/e/o/i/l/p):
//
//	c + kind(1)                  -> uint64   contatore (kind: n,e,L,T,K)
//	L + name                     -> id(4)    dizionario label (name→id)
//	T + name                     -> id(4)    dizionario tipi
//	K + name                     -> id(4)    dizionario chiavi-proprietà
//	R + kind(1) + id(4)          -> name     reverse dei dizionari
//	X + label(4) + propKey(4)    -> {}       registry indici `p` attivi
//
// Gli ID partono da 1; l'ID 0 è riservato come "nessuno".
package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/mycypher/internal/storage"
)

const (
	tagCounter  byte = 'c'
	tagLabelFwd byte = 'L'
	tagTypeFwd  byte = 'T'
	tagKeyFwd   byte = 'K'
	tagReverse  byte = 'R'
	tagIndexReg byte = 'X'
)

// kind dei dizionari, usato per contatore e reverse.
const (
	kindLabel byte = 'L'
	kindType  byte = 'T'
	kindKey   byte = 'K'
	kindNode  byte = 'n'
	kindEdge  byte = 'e'
)

// InternLabel restituisce l'ID della label, allocandolo se nuovo (idempotente).
func InternLabel(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagLabelFwd, kindLabel, name)
}

// InternType restituisce l'ID del tipo di relazione, allocandolo se nuovo.
func InternType(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagTypeFwd, kindType, name)
}

// InternKey restituisce l'ID della chiave-proprietà, allocandolo se nuovo.
func InternKey(txn storage.Txn, name string) (uint32, error) {
	return internDict(txn, tagKeyFwd, kindKey, name)
}

// LabelName risolve l'ID di una label nel suo nome.
func LabelName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindLabel, id) }

// TypeName risolve l'ID di un tipo nel suo nome.
func TypeName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindType, id) }

// KeyName risolve l'ID di una chiave-proprietà nel suo nome.
func KeyName(txn storage.Txn, id uint32) (string, error) { return revName(txn, kindKey, id) }

// NextNodeID alloca un nuovo ID nodo monotòno.
func NextNodeID(txn storage.Txn) (uint64, error) { return nextCounter(txn, kindNode) }

// NextEdgeID alloca un nuovo ID arco monotòno.
func NextEdgeID(txn storage.Txn) (uint64, error) { return nextCounter(txn, kindEdge) }

func internDict(txn storage.Txn, fwdTag, kind byte, name string) (uint32, error) {
	fwd := append([]byte{fwdTag}, name...)
	switch v, err := txn.Get(fwd); {
	case err == nil:
		return binary.BigEndian.Uint32(v), nil
	case !errors.Is(err, storage.ErrNotFound):
		return 0, err
	}

	id64, err := nextCounter(txn, kind)
	if err != nil {
		return 0, err
	}
	if id64 > 0xFFFFFFFF {
		return 0, fmt.Errorf("catalog: esauriti gli ID per il dizionario %c", kind)
	}
	id := uint32(id64)

	var idb [4]byte
	binary.BigEndian.PutUint32(idb[:], id)
	if err := txn.Set(fwd, idb[:]); err != nil {
		return 0, err
	}
	rev := append([]byte{tagReverse, kind}, idb[:]...)
	if err := txn.Set(rev, []byte(name)); err != nil {
		return 0, err
	}
	return id, nil
}

func revName(txn storage.Txn, kind byte, id uint32) (string, error) {
	var idb [4]byte
	binary.BigEndian.PutUint32(idb[:], id)
	key := append([]byte{tagReverse, kind}, idb[:]...)
	v, err := txn.Get(key)
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// nextCounter incrementa il contatore di tipo kind e restituisce il nuovo valore
// (pre-incremento: 1, 2, 3, ...).
func nextCounter(txn storage.Txn, kind byte) (uint64, error) {
	key := []byte{tagCounter, kind}
	var cur uint64
	switch v, err := txn.Get(key); {
	case err == nil:
		cur = binary.BigEndian.Uint64(v)
	case !errors.Is(err, storage.ErrNotFound):
		return 0, err
	}
	next := cur + 1
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], next)
	if err := txn.Set(key, b[:]); err != nil {
		return 0, err
	}
	return next, nil
}

// IndexDef identifica un indice secondario `p` su (label, propKey).
type IndexDef struct {
	Label   uint32
	PropKey uint32
}

func indexKey(label, propKey uint32) []byte {
	b := make([]byte, 1+4+4)
	b[0] = tagIndexReg
	binary.BigEndian.PutUint32(b[1:5], label)
	binary.BigEndian.PutUint32(b[5:9], propKey)
	return b
}

// AddIndex registra un indice `p` attivo (idempotente).
func AddIndex(txn storage.Txn, label, propKey uint32) error {
	return txn.Set(indexKey(label, propKey), []byte{})
}

// HasIndex indica se esiste un indice `p` su (label, propKey).
func HasIndex(txn storage.Txn, label, propKey uint32) (bool, error) {
	switch _, err := txn.Get(indexKey(label, propKey)); {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// ListIndexes elenca gli indici `p` registrati.
func ListIndexes(txn storage.Txn) ([]IndexDef, error) {
	var out []IndexDef
	it := txn.Scan([]byte{tagIndexReg})
	defer func() { _ = it.Close() }()
	for ; it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 1+4+4 {
			return nil, fmt.Errorf("catalog: chiave registry indici malformata (len=%d)", len(k))
		}
		out = append(out, IndexDef{
			Label:   binary.BigEndian.Uint32(k[1:5]),
			PropKey: binary.BigEndian.Uint32(k[5:9]),
		})
	}
	return out, nil
}
