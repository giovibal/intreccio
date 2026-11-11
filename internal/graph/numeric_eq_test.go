package graph

import (
	"math"
	"sort"
	"testing"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
)

// TestNodesByPropertyNumericEquality checks that an equality lookup returns the
// same nodes whether or not a `p` index exists, and that it follows the
// query-level numeric equality: 1 = 1.0, 0.0 = -0.0, NaN equals nothing.
func TestNodesByPropertyNumericEquality(t *testing.T) {
	big := int64(1) << 60 // beyond 2^53: several int64 share one float64
	values := []any{
		int64(1), float64(1), float64(1.5), int64(0), float64(0), math.Copysign(0, -1),
		math.NaN(), "1", true, big, big + 1, float64(big),
	}
	probes := []any{
		int64(1), float64(1), float64(1.5), int64(0), float64(0), math.Copysign(0, -1),
		math.NaN(), "1", true, big, big + 1, float64(big), int64(2),
	}
	for _, indexed := range []bool{false, true} {
		s := newStore(t)
		var labelID, keyID uint32
		ids := make([]uint64, len(values))
		mustUpdate(t, s, func(tx storage.Txn) error {
			var err error
			if labelID, err = catalog.InternLabel(tx, "P"); err != nil {
				return err
			}
			if keyID, err = catalog.InternKey(tx, "x"); err != nil {
				return err
			}
			if indexed {
				if err := catalog.AddIndex(tx, labelID, keyID); err != nil {
					return err
				}
			}
			for i, v := range values {
				if ids[i], err = CreateNode(tx, []string{"P"}, map[string]any{"x": v}); err != nil {
					return err
				}
			}
			return nil
		})
		for _, p := range probes {
			var want []uint64
			for i, v := range values {
				if QueryEqual(v, p) {
					want = append(want, ids[i])
				}
			}
			mustView(t, s, func(tx storage.Txn) error {
				got, err := NodesByProperty(tx, labelID, keyID, p)
				if err != nil {
					return err
				}
				sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
				if !equalIDs(got, want) {
					t.Errorf("indexed=%v probe %v (%T): got %v, want %v", indexed, p, p, got, want)
				}
				return nil
			})
		}
	}
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueryEqual(t *testing.T) {
	negZero := math.Copysign(0, -1)
	cases := []struct {
		a, b any
		want bool
	}{
		{int64(1), float64(1), true},
		{float64(1), int64(1), true},
		{int64(1), float64(1.5), false},
		{float64(0), negZero, true},
		{int64(0), negZero, true},
		{math.NaN(), math.NaN(), false},
		{math.NaN(), int64(0), false},
		{"a", "a", true},
		{"1", int64(1), false},
		{true, true, true},
		{true, int64(1), false},
		{nil, nil, false},
		{int64(1), nil, false},
	}
	for _, c := range cases {
		if got := QueryEqual(c.a, c.b); got != c.want {
			t.Errorf("QueryEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestSetPropertySignedZeroKeepsIndexConsistent: 0.0 and -0.0 have different
// index keys, so switching between them must replace the `p` entry.
func TestSetPropertySignedZeroKeepsIndexConsistent(t *testing.T) {
	for _, replace := range []bool{false, true} {
		s := newStore(t)
		var nid uint64
		mustUpdate(t, s, func(tx storage.Txn) error {
			lid, err := catalog.InternLabel(tx, "P")
			if err != nil {
				return err
			}
			kid, err := catalog.InternKey(tx, "x")
			if err != nil {
				return err
			}
			if err := catalog.AddIndex(tx, lid, kid); err != nil {
				return err
			}
			nid, err = CreateNode(tx, []string{"P"}, map[string]any{"x": float64(0)})
			return err
		})
		mustUpdate(t, s, func(tx storage.Txn) error {
			return SetNodeProperties(tx, nid, map[string]any{"x": math.Copysign(0, -1)}, replace)
		})
		mustView(t, s, func(tx storage.Txn) error {
			assertNodeIndexConsistency(t, tx, nid)
			return nil
		})
	}
}
