package graph

import (
	"sort"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// putRelPropEntry writes the `r` index entry of value for (type, propKey) on
// edge, if that pair is indexed and the value indexable. Every `r` write goes
// through it.
func putRelPropEntry(txn storage.Txn, typeID, propKey uint32, value any, edge uint64) error {
	if !isIndexable(value) {
		return nil
	}
	has, err := catalog.HasRelIndex(txn, typeID, propKey)
	if err != nil || !has {
		return err
	}
	k, err := codec.RelPropKey(typeID, propKey, value, edge)
	if err != nil {
		return err
	}
	return txn.Set(k, nil)
}

// delRelPropEntry is the inverse of putRelPropEntry.
func delRelPropEntry(txn storage.Txn, typeID, propKey uint32, value any, edge uint64) error {
	if !isIndexable(value) {
		return nil
	}
	has, err := catalog.HasRelIndex(txn, typeID, propKey)
	if err != nil || !has {
		return err
	}
	k, err := codec.RelPropKey(typeID, propKey, value, edge)
	if err != nil {
		return err
	}
	return txn.Delete(k)
}

// BuildRelIndex registers the relationship index on (type, propKey) under
// name and backfills an entry for every existing relationship of that type.
// The backfill reads every edge record once (there is no index by type).
func BuildRelIndex(txn storage.Txn, typeID, propKey uint32, name string) error {
	if err := catalog.RequireRelIndexFormat(txn); err != nil {
		return err
	}
	if err := catalog.AddRelIndex(txn, typeID, propKey, name); err != nil {
		return err
	}
	return ScanEdgesOfType(txn, typeID, func(e Edge) error {
		v, ok := e.Props[propKey]
		if !ok || !isIndexable(v) {
			return nil
		}
		k, err := codec.RelPropKey(typeID, propKey, v, e.ID)
		if err != nil {
			return err
		}
		return txn.Set(k, nil)
	})
}

// DropRelIndexEntries removes every entry of the (type, propKey) relationship
// index and unregisters it.
func DropRelIndexEntries(txn storage.Txn, typeID, propKey uint32) error {
	if err := deletePrefix(txn, codec.RelPropIndexPrefix(typeID, propKey)); err != nil {
		return err
	}
	return catalog.RemoveRelIndex(txn, typeID, propKey)
}

// EdgesByProperty returns, in ID order, the IDs of the relationships of the
// type whose propKey equals value (as Cypher's = does: 1 matches 1.0). It
// uses the relationship index when it can, and otherwise (no index, or a
// value whose equal values cannot be enumerated as keys) scans the edge
// records.
func EdgesByProperty(txn storage.Txn, typeID, propKey uint32, value any) ([]uint64, error) {
	value = normalize(value)
	if value == nil {
		return nil, nil // = null matches nothing
	}
	has, err := catalog.HasRelIndex(txn, typeID, propKey)
	if err != nil {
		return nil, err
	}
	probes, exact := equalityProbes(value)
	if !has || !exact || !isIndexable(value) {
		return edgesByScan(txn, typeID, propKey, value)
	}
	var ids []uint64
	for _, p := range probes {
		prefix, err := codec.RelPropPrefix(typeID, propKey, p)
		if err != nil {
			return nil, err
		}
		it := txn.Scan(prefix)
		for ; it.Valid(); it.Next() {
			_, id, err := codec.ParseRelPropKey(it.Key())
			if err != nil {
				_ = it.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := it.Close(); err != nil {
			return nil, err
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// edgesByScan is EdgesByProperty without the index.
func edgesByScan(txn storage.Txn, typeID, propKey uint32, value any) ([]uint64, error) {
	var ids []uint64
	err := ScanEdgesOfType(txn, typeID, func(e Edge) error {
		if v, ok := e.Props[propKey]; ok && QueryEqual(v, value) {
			ids = append(ids, e.ID)
		}
		return nil
	})
	return ids, err
}

// ScanEdgesOfType calls fn for every relationship of the type, in ID order.
// There is no index by type, so it reads every edge record.
func ScanEdgesOfType(txn storage.Txn, typeID uint32, fn func(Edge) error) error {
	var edges []Edge
	it := txn.Scan(codec.EdgePrefix())
	for ; it.Valid(); it.Next() {
		b, err := it.Value()
		if err == nil {
			var rec codec.EdgeRecord
			if rec, err = codec.DecodeEdge(b); err == nil && rec.Type == typeID {
				var id uint64
				if id, err = codec.EdgeIDFromKey(it.Key()); err == nil {
					edges = append(edges, Edge{ID: id, EdgeRecord: rec})
				}
			}
		}
		if err != nil {
			_ = it.Close()
			return err
		}
	}
	if err := it.Close(); err != nil {
		return err
	}
	for _, e := range edges {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}
