// Package graph è il modello + CRUD transazionale + primitive di traversal.
//
// È l'unico punto da cui passano le scritture: ogni mutazione aggiorna il record
// base e tutte le sue chiavi-indice (`l`, `p`, `o`, `i`) nella stessa transazione
// (Invariante #1). Le funzioni operano su una storage.Txn fornita dal chiamante,
// così più mutazioni possono comporsi atomicamente in un'unica Update.
package graph

import (
	"errors"
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/storage"
	"github.com/giovibal/mycypher/internal/storage/codec"
)

// CreateNode crea un nodo con le label e proprietà date (per nome), internandone
// gli identificatori, e ne aggiorna gli indici. Restituisce il nuovo nodeID.
func CreateNode(txn storage.Txn, labels []string, props map[string]any) (uint64, error) {
	labelIDs := make([]uint32, 0, len(labels))
	for _, name := range labels {
		id, err := catalog.InternLabel(txn, name)
		if err != nil {
			return 0, err
		}
		labelIDs = append(labelIDs, id)
	}
	p, err := internProps(txn, props)
	if err != nil {
		return 0, err
	}
	id, err := catalog.NextNodeID(txn)
	if err != nil {
		return 0, err
	}
	rec := codec.NodeRecord{Labels: labelIDs, Props: p}
	if err := putNodeRecord(txn, id, rec); err != nil {
		return 0, err
	}
	if err := putNodeIndexes(txn, id, rec); err != nil {
		return 0, err
	}
	return id, nil
}

// CreateEdge crea un arco tipizzato tra due nodi esistenti, scrivendo il record
// `e` e le due viste di adiacenza `o`/`i` (Invariante #4).
func CreateEdge(txn storage.Txn, typ string, src, dst uint64, props map[string]any) (uint64, error) {
	if _, err := GetNode(txn, src); err != nil {
		return 0, fmt.Errorf("arco src %d: %w", src, err)
	}
	if _, err := GetNode(txn, dst); err != nil {
		return 0, fmt.Errorf("arco dst %d: %w", dst, err)
	}
	typeID, err := catalog.InternType(txn, typ)
	if err != nil {
		return 0, err
	}
	p, err := internProps(txn, props)
	if err != nil {
		return 0, err
	}
	id, err := catalog.NextEdgeID(txn)
	if err != nil {
		return 0, err
	}
	rec := codec.EdgeRecord{Type: typeID, Src: src, Dst: dst, Props: p}
	b, err := codec.EncodeEdge(rec)
	if err != nil {
		return 0, err
	}
	if err := txn.Set(codec.EdgeKey(id), b); err != nil {
		return 0, err
	}
	if err := txn.Set(codec.OutKey(src, typeID, dst, id), nil); err != nil {
		return 0, err
	}
	if err := txn.Set(codec.InKey(dst, typeID, src, id), nil); err != nil {
		return 0, err
	}
	return id, nil
}

// SetProperty imposta (o sostituisce) una proprietà del nodo, aggiornando il
// record e le entry dell'indice `p` per le label indicizzate (delta vecchio→nuovo).
func SetProperty(txn storage.Txn, nodeID uint64, key string, value any) error {
	node, err := GetNode(txn, nodeID)
	if err != nil {
		return err
	}
	keyID, err := catalog.InternKey(txn, key)
	if err != nil {
		return err
	}
	value = normalize(value)
	old, had := node.Props[keyID]

	for _, l := range node.Labels {
		has, err := catalog.HasIndex(txn, l, keyID)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		if had && isIndexable(old) {
			ok, err := codec.PropKey(l, keyID, old, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Delete(ok); err != nil {
				return err
			}
		}
		if isIndexable(value) {
			nk, err := codec.PropKey(l, keyID, value, nodeID)
			if err != nil {
				return err
			}
			if err := txn.Set(nk, nil); err != nil {
				return err
			}
		}
	}

	node.Props[keyID] = value
	return putNodeRecord(txn, nodeID, node.NodeRecord)
}

// GetNode legge un nodo. Restituisce ErrNodeNotFound se assente.
func GetNode(txn storage.Txn, id uint64) (Node, error) {
	b, err := txn.Get(codec.NodeKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		return Node{}, ErrNodeNotFound
	}
	if err != nil {
		return Node{}, err
	}
	rec, err := codec.DecodeNode(b)
	if err != nil {
		return Node{}, fmt.Errorf("graph: decode nodo %d: %w", id, err)
	}
	return Node{ID: id, NodeRecord: rec}, nil
}

// GetEdge legge un arco. Restituisce ErrEdgeNotFound se assente.
func GetEdge(txn storage.Txn, id uint64) (Edge, error) {
	b, err := txn.Get(codec.EdgeKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		return Edge{}, ErrEdgeNotFound
	}
	if err != nil {
		return Edge{}, err
	}
	rec, err := codec.DecodeEdge(b)
	if err != nil {
		return Edge{}, fmt.Errorf("graph: decode arco %d: %w", id, err)
	}
	return Edge{ID: id, EdgeRecord: rec}, nil
}

// DeleteEdge rimuove un arco: record `e` più le due entry di adiacenza `o`/`i`.
func DeleteEdge(txn storage.Txn, id uint64) error {
	e, err := GetEdge(txn, id)
	if err != nil {
		return err
	}
	if err := txn.Delete(codec.OutKey(e.Src, e.Type, e.Dst, id)); err != nil {
		return err
	}
	if err := txn.Delete(codec.InKey(e.Dst, e.Type, e.Src, id)); err != nil {
		return err
	}
	return txn.Delete(codec.EdgeKey(id))
}

// DeleteNode rimuove un nodo e tutte le sue entry indice (`l`, `p`). Restituisce
// ErrNodeHasEdges se esistono archi incidenti: il chiamante deve rimuoverli prima
// (la semantica DETACH DELETE arriverà in Fase 7).
func DeleteNode(txn storage.Txn, id uint64) error {
	node, err := GetNode(txn, id)
	if err != nil {
		return err
	}
	has, err := hasIncidentEdges(txn, id)
	if err != nil {
		return err
	}
	if has {
		return ErrNodeHasEdges
	}
	if err := delNodeIndexes(txn, id, node.NodeRecord); err != nil {
		return err
	}
	return txn.Delete(codec.NodeKey(id))
}

func internProps(txn storage.Txn, props map[string]any) (map[uint32]any, error) {
	out := make(map[uint32]any, len(props))
	for k, v := range props {
		id, err := catalog.InternKey(txn, k)
		if err != nil {
			return nil, err
		}
		out[id] = normalize(v)
	}
	return out, nil
}

func putNodeRecord(txn storage.Txn, id uint64, rec codec.NodeRecord) error {
	b, err := codec.EncodeNode(rec)
	if err != nil {
		return err
	}
	return txn.Set(codec.NodeKey(id), b)
}

// putNodeIndexes scrive le entry `l` (una per label) e `p` (per ogni coppia
// label×propKey indicizzata con valore scalare presente).
func putNodeIndexes(txn storage.Txn, nodeID uint64, rec codec.NodeRecord) error {
	return forEachNodeIndex(txn, rec, func(key []byte) error { return txn.Set(key, nil) },
		func(label uint32) error { return txn.Set(codec.LabelKey(label, nodeID), nil) },
		nodeID)
}

// delNodeIndexes rimuove le stesse entry scritte da putNodeIndexes.
func delNodeIndexes(txn storage.Txn, nodeID uint64, rec codec.NodeRecord) error {
	return forEachNodeIndex(txn, rec, func(key []byte) error { return txn.Delete(key) },
		func(label uint32) error { return txn.Delete(codec.LabelKey(label, nodeID)) },
		nodeID)
}

// forEachNodeIndex applica onLabel per ogni label e onProp per ogni entry `p` da
// mantenere, così put/del condividono l'enumerazione (un solo punto di verità).
func forEachNodeIndex(txn storage.Txn, rec codec.NodeRecord, onProp func(key []byte) error, onLabel func(label uint32) error, nodeID uint64) error {
	for _, l := range rec.Labels {
		if err := onLabel(l); err != nil {
			return err
		}
		for k, v := range rec.Props {
			if !isIndexable(v) {
				continue
			}
			has, err := catalog.HasIndex(txn, l, k)
			if err != nil {
				return err
			}
			if !has {
				continue
			}
			pk, err := codec.PropKey(l, k, v, nodeID)
			if err != nil {
				return err
			}
			if err := onProp(pk); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasIncidentEdges(txn storage.Txn, nodeID uint64) (bool, error) {
	for _, prefix := range [][]byte{codec.OutPrefixAll(nodeID), codec.InPrefixAll(nodeID)} {
		it := txn.Scan(prefix)
		valid := it.Valid()
		if err := it.Close(); err != nil {
			return false, err
		}
		if valid {
			return true, nil
		}
	}
	return false, nil
}
