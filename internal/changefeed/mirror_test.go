package changefeed

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"testing/quick"
)

func TestMirrorReset(t *testing.T) {
	cases := []struct {
		name        string
		held, snap  []string
		wantAdded   []int
		wantRemoved []string
	}{
		{"first snapshot", nil, []string{"b", "a"}, []int{0, 1}, nil},
		{"identical", []string{"a", "b"}, []string{"b", "a"}, nil, nil},
		{"gained", []string{"a"}, []string{"c", "a", "b"}, []int{0, 2}, nil},
		{"lost, in byte order", []string{"c", "a", "b"}, []string{"a"}, nil, []string{"b", "c"}},
		{"replaced", []string{"a"}, []string{"b"}, []int{0}, []string{"a"}},
		{"emptied", []string{"a", "a"}, nil, nil, []string{"a", "a"}},
		{"duplicates", []string{"a", "a", "b"}, []string{"a", "a", "a"}, []int{2}, []string{"b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m Mirror
			m.Apply(rows(c.held...), nil)
			added, removed := m.Reset(rows(c.snap...))
			if !reflect.DeepEqual(added, c.wantAdded) {
				t.Errorf("added = %v, want %v", added, c.wantAdded)
			}
			var got []string
			if removed != nil {
				got = strs(removed)
			}
			if !reflect.DeepEqual(got, c.wantRemoved) {
				t.Errorf("removed = %v, want %v", got, c.wantRemoved)
			}
			if m.Len() != len(c.snap) {
				t.Errorf("Len = %d, want %d", m.Len(), len(c.snap))
			}
		})
	}
}

// TestMirrorFollowsResultSet is the property behind transparent failover: a
// mirror fed the batches of a ResultSet holds the same rows, so resetting it
// to the current result is a no-op, and resetting it to any other result
// reports exactly the difference.
func TestMirrorFollowsResultSet(t *testing.T) {
	gen := func(r *rand.Rand) [][]byte {
		out := make([][]byte, r.Intn(8))
		for i := range out {
			out[i] = []byte{byte('a' + r.Intn(5))}
		}
		return out
	}
	multiset := func(rs [][]byte) []string {
		out := strs(rs)
		sort.Strings(out)
		return out
	}
	prop := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var (
			rs ResultSet
			m  Mirror
		)
		cur := gen(r)
		for range 10 {
			added, removed := rs.Replace(cur)
			batch := make([][]byte, len(added))
			for i, idx := range added {
				batch[i] = cur[idx]
			}
			m.Apply(batch, removed)

			check := m // resetting a copy to the same result must change nothing
			check.counts = map[string]int{}
			for k, v := range m.counts {
				check.counts[k] = v
			}
			if a, rm := check.Reset(cur); len(a) != 0 || len(rm) != 0 {
				t.Logf("seed %d: mirror diverged from the result: +%v -%v", seed, a, strs(rm))
				return false
			}
			cur = gen(r)
		}

		// Jump to an unrelated result: what Reset reports turns the old rows
		// into the new ones.
		before := multiset(rs.rows)
		next := gen(r)
		added, removed := m.Reset(next)
		got := append([]string(nil), before...)
		for _, row := range removed {
			i := sort.SearchStrings(got, string(row))
			if i >= len(got) || got[i] != string(row) {
				t.Logf("seed %d: removed a row that was not held", seed)
				return false
			}
			got = append(got[:i], got[i+1:]...)
		}
		for _, idx := range added {
			got = append(got, string(next[idx]))
		}
		sort.Strings(got)
		return reflect.DeepEqual(got, multiset(next)) || (len(got) == 0 && len(next) == 0)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}
