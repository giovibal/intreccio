package graph

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// ErrConstraintViolation is returned when a write would give two nodes the
// same value for a property under a unique constraint.
var ErrConstraintViolation = errors.New("graph: unique constraint violation")

// putPropEntry writes the `p` index entry of value for (label, propKey) on
// nodeID, if that pair is indexed and the value indexable, and claims the
// value when the pair is unique-constrained. Every `p` write goes through it.
func putPropEntry(txn storage.Txn, label, propKey uint32, value any, nodeID uint64) error {
	if !isIndexable(value) {
		return nil
	}
	has, err := catalog.HasIndex(txn, label, propKey)
	if err != nil || !has {
		return err
	}
	pk, err := codec.PropKey(label, propKey, value, nodeID)
	if err != nil {
		return err
	}
	if err := txn.Set(pk, nil); err != nil {
		return err
	}
	unique, err := catalog.HasUnique(txn, label, propKey)
	if err != nil || !unique {
		return err
	}
	return claimValue(txn, label, propKey, value, nodeID)
}

// delPropEntry is the inverse of putPropEntry: it removes the `p` entry and
// releases the value if nodeID holds it.
func delPropEntry(txn storage.Txn, label, propKey uint32, value any, nodeID uint64) error {
	if !isIndexable(value) {
		return nil
	}
	has, err := catalog.HasIndex(txn, label, propKey)
	if err != nil || !has {
		return err
	}
	pk, err := codec.PropKey(label, propKey, value, nodeID)
	if err != nil {
		return err
	}
	if err := txn.Delete(pk); err != nil {
		return err
	}
	unique, err := catalog.HasUnique(txn, label, propKey)
	if err != nil || !unique {
		return err
	}
	canon, ok := uniqueValue(value)
	if !ok {
		return nil
	}
	uk, err := codec.UniqueKey(label, propKey, canon)
	if err != nil {
		return err
	}
	owner, found, err := valueOwner(txn, uk)
	if err != nil || !found || owner != nodeID {
		return err
	}
	return txn.Delete(uk)
}

// uniqueValue maps value to the form its owner key is built from, so values
// that are equal in Cypher share a key: an integral float within the int64
// range counts as that integer (1.0 and 1 collide). ok is false for NaN,
// which equals nothing and so never conflicts.
func uniqueValue(value any) (any, bool) {
	f, isFloat := value.(float64)
	if !isFloat {
		return value, true
	}
	switch {
	case math.IsNaN(f):
		return nil, false
	case f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64:
		return int64(f), true // also maps -0.0 to 0
	}
	return f, true
}

// claimValue records nodeID as the holder of value, failing if another node
// holds it. Reading the owner key makes concurrent claims of the same value
// conflict (and retry) rather than both succeed.
func claimValue(txn storage.Txn, label, propKey uint32, value any, nodeID uint64) error {
	canon, ok := uniqueValue(value)
	if !ok {
		return nil
	}
	uk, err := codec.UniqueKey(label, propKey, canon)
	if err != nil {
		return err
	}
	owner, found, err := valueOwner(txn, uk)
	if err != nil {
		return err
	}
	if found && owner != nodeID {
		return violation(txn, label, propKey, value)
	}
	if found {
		return nil
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], nodeID)
	return txn.Set(uk, b[:])
}

func valueOwner(txn storage.Txn, uk []byte) (uint64, bool, error) {
	v, err := txn.Get(uk)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	case len(v) != 8:
		return 0, false, fmt.Errorf("graph: malformed unique owner entry (len=%d)", len(v))
	}
	return binary.BigEndian.Uint64(v), true, nil
}

func violation(txn storage.Txn, label, propKey uint32, value any) error {
	l, err := catalog.LabelName(txn, label)
	if err != nil {
		return err
	}
	k, err := catalog.KeyName(txn, propKey)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: another :%s node already has %s = %s", ErrConstraintViolation, l, k, formatValue(value))
}

func formatValue(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(v)
}

// BuildIndex registers the `p` index on (label, propKey) under name and
// backfills an entry for every node carrying the label.
func BuildIndex(txn storage.Txn, label, propKey uint32, name string) error {
	if err := catalog.AddNamedIndex(txn, label, propKey, name); err != nil {
		return err
	}
	return forEachLabelValue(txn, label, propKey, func(nodeID uint64, v any) error {
		pk, err := codec.PropKey(label, propKey, v, nodeID)
		if err != nil {
			return err
		}
		return txn.Set(pk, nil)
	})
}

// AddUniqueConstraint registers a unique constraint on (label, propKey),
// creating its backing `p` index if missing, and claims the current values.
// It fails with ErrConstraintViolation if two nodes already share a value.
func AddUniqueConstraint(txn storage.Txn, label, propKey uint32, name string) error {
	hasIndex, err := catalog.HasIndex(txn, label, propKey)
	if err != nil {
		return err
	}
	if !hasIndex {
		if err := BuildIndex(txn, label, propKey, ""); err != nil {
			return err
		}
	}
	if err := catalog.RequireUniqueFormat(txn); err != nil {
		return err
	}
	if err := catalog.AddConstraint(txn, catalog.ConstraintDef{
		Label: label, PropKey: propKey, Name: name, OwnsIndex: !hasIndex,
	}); err != nil {
		return err
	}
	return forEachLabelValue(txn, label, propKey, func(nodeID uint64, v any) error {
		return claimValue(txn, label, propKey, v, nodeID)
	})
}

// DropUniqueConstraint removes the constraint on (label, propKey), its owner
// entries and, when it created it, its backing index.
func DropUniqueConstraint(txn storage.Txn, label, propKey uint32) error {
	def, found, err := catalog.GetConstraint(txn, label, propKey)
	if err != nil || !found {
		return err
	}
	if err := deletePrefix(txn, codec.UniquePrefix(label, propKey)); err != nil {
		return err
	}
	if err := catalog.RemoveConstraint(txn, label, propKey); err != nil {
		return err
	}
	if def.OwnsIndex {
		return DropIndexEntries(txn, label, propKey)
	}
	return nil
}

// forEachLabelValue calls fn for every node with the label whose propKey
// value is indexable.
func forEachLabelValue(txn storage.Txn, label, propKey uint32, fn func(nodeID uint64, v any) error) error {
	nodeIDs, err := NodesByLabel(txn, label)
	if err != nil {
		return err
	}
	for _, id := range nodeIDs {
		node, err := GetNode(txn, id)
		if err != nil {
			return err
		}
		v, ok := node.Props[propKey]
		if !ok || !isIndexable(v) {
			continue
		}
		if err := fn(id, v); err != nil {
			return err
		}
	}
	return nil
}

// deletePrefix deletes every key under prefix.
func deletePrefix(txn storage.Txn, prefix []byte) error {
	var keys [][]byte
	it := txn.Scan(prefix)
	for ; it.Valid(); it.Next() {
		keys = append(keys, it.Key())
	}
	if err := it.Close(); err != nil {
		return err
	}
	for _, k := range keys {
		if err := txn.Delete(k); err != nil {
			return err
		}
	}
	return nil
}
