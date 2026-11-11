package overlay

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

type entry struct {
	val string
	del bool
}

// TestTreeMatchesMap applies random puts and deletes to a Tree and a map and
// checks Get, Len, ordered iteration from random starts, and that every older
// snapshot still reads as it did.
func TestTreeMatchesMap(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	var tree Tree
	model := map[string]entry{}
	type snap struct {
		tree  Snapshot
		n     int
		model map[string]entry
	}
	var snaps []snap
	for i := range 3000 {
		k := fmt.Sprintf("k%03d", r.IntN(400))
		if r.IntN(4) == 0 {
			tree.Delete([]byte(k))
			model[k] = entry{del: true}
		} else {
			v := fmt.Sprint(i)
			tree.Put([]byte(k), []byte(v))
			model[k] = entry{val: v}
		}
		if i%300 == 0 {
			snaps = append(snaps, snap{tree.Snapshot(), tree.Len(), cloneMap(model)})
		}
	}
	snaps = append(snaps, snap{tree.Snapshot(), tree.Len(), model})
	for si, s := range snaps {
		if s.n != len(s.model) {
			t.Fatalf("snapshot %d: Len = %d, want %d", si, s.n, len(s.model))
		}
		for k, e := range s.model {
			v, del, found := s.tree.Get([]byte(k))
			if !found || del != e.del || (!del && string(v) != e.val) {
				t.Fatalf("snapshot %d: Get(%s) = %q %v %v, want %+v", si, k, v, del, found, e)
			}
		}
		if _, _, found := s.tree.Get([]byte("nope")); found {
			t.Fatalf("snapshot %d: found a missing key", si)
		}
		keys := make([]string, 0, len(s.model))
		for k := range s.model {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, start := range []string{"", "k000", "k2", "k250", "k399", "z"} {
			var want, got []string
			for _, k := range keys {
				if k >= start {
					want = append(want, k)
				}
			}
			for it := s.tree.Seek([]byte(start)); it.Valid(); it.Next() {
				got = append(got, string(it.Key()))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("snapshot %d: Seek(%q) = %v, want %v", si, start, got, want)
			}
		}
	}
}

func cloneMap(m map[string]entry) map[string]entry {
	out := make(map[string]entry, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestTreeDepth: the treap stays shallow even when keys arrive in order.
func TestTreeDepth(t *testing.T) {
	var tree Tree
	for i := range 100000 {
		tree.Put(fmt.Appendf(nil, "%08d", i), nil)
	}
	var depth func(n *node) int
	depth = func(n *node) int {
		if n == nil {
			return 0
		}
		return 1 + max(depth(n.left), depth(n.right))
	}
	if d := depth(tree.root); d > 60 {
		t.Errorf("depth = %d for 100k sequential keys, want O(log n)", d)
	}
}

// sliceSource is a Source over sorted committed keys.
type sliceSource struct {
	keys []string
	pos  int
}

func (s *sliceSource) Valid() bool            { return s.pos < len(s.keys) }
func (s *sliceSource) Key() []byte            { return []byte(s.keys[s.pos]) }
func (s *sliceSource) Value() ([]byte, error) { return []byte("c"), nil }
func (s *sliceSource) Next()                  { s.pos++ }
func (s *sliceSource) Close() error           { return nil }

func TestMerged(t *testing.T) {
	var tree Tree
	tree.Put([]byte("p2"), []byte("u")) // overrides committed
	tree.Delete([]byte("p3"))           // hides committed
	tree.Put([]byte("p0"), []byte("n")) // new, first
	tree.Put([]byte("p9"), []byte("n")) // new, last
	tree.Delete([]byte("p5"))           // deletion of a missing key
	tree.Put([]byte("q1"), []byte("x")) // outside the prefix
	base := &sliceSource{keys: []string{"p1", "p2", "p3", "p4"}}
	m := NewMerged(base, tree.Seek([]byte("p")), []byte("p"))
	tree.Put([]byte("p1"), []byte("later")) // after the iterator: invisible to it
	tree.Put([]byte("p1"), []byte("later")) // after the iterator: invisible to it
	var got []string
	for ; m.Valid(); m.Next() {
		v, _ := m.Value()
		got = append(got, string(m.Key())+"="+string(v))
	}
	want := []string{"p0=n", "p1=c", "p2=u", "p4=c", "p9=n"}
	if !slices.Equal(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}
