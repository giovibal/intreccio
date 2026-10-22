package graph

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
)

type testEdge struct {
	id       uint64
	typ      string
	src, dst uint64
}

// bruteShortest enumerates every relationship-distinct walk from src to dst of
// at most maxLen hops and keeps the shortest ones, as edge-ID sequences.
func bruteShortest(edges []testEdge, src, dst uint64, out, in bool, types map[string]bool, maxLen int) []string {
	var best []string
	bestLen := -1
	var walk func(n uint64, used map[uint64]bool, seq []uint64)
	walk = func(n uint64, used map[uint64]bool, seq []uint64) {
		if bestLen >= 0 && len(seq) > bestLen {
			return
		}
		if n == dst && len(seq) > 0 {
			if bestLen < 0 || len(seq) < bestLen {
				bestLen, best = len(seq), nil
			}
			best = append(best, fmt.Sprint(seq))
			return
		}
		if len(seq) == maxLen {
			return
		}
		for _, e := range edges {
			if used[e.id] || (types != nil && !types[e.typ]) || e.src == e.dst {
				continue
			}
			var m uint64
			switch {
			case out && e.src == n:
				m = e.dst
			case in && e.dst == n:
				m = e.src
			default:
				continue
			}
			used[e.id] = true
			walk(m, used, append(seq, e.id))
			delete(used, e.id)
		}
	}
	walk(src, map[uint64]bool{}, nil)
	sort.Strings(best)
	return best
}

func pathIDs(ps []Path) []string {
	var out []string
	for _, p := range ps {
		ids := make([]uint64, len(p.Rels))
		for i, r := range p.Rels {
			ids[i] = r.ID
		}
		out = append(out, fmt.Sprint(ids))
	}
	sort.Strings(out)
	return out
}

// TestShortestPathsMatchesBruteForce compares the bidirectional search with an
// exhaustive enumeration on random small multigraphs, in every direction mode.
func TestShortestPathsMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for round := range 40 {
		s := newStore(t)
		const nodes = 9
		var edges []testEdge
		err := s.Update(func(tx storage.Txn) error {
			ids := make([]uint64, nodes)
			for i := range ids {
				id, err := CreateNode(tx, []string{"N"}, nil)
				if err != nil {
					return err
				}
				ids[i] = id
			}
			for range 4 + r.IntN(14) {
				src, dst := ids[r.IntN(nodes)], ids[r.IntN(nodes)]
				typ := []string{"A", "B"}[r.IntN(2)]
				id, err := CreateEdge(tx, typ, src, dst, nil)
				if err != nil {
					return err
				}
				edges = append(edges, testEdge{id, typ, src, dst})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		typeA, foundA, err := typeIDOf(s, "A")
		if err != nil {
			t.Fatal(err)
		}
		idsA := []uint32{typeA}
		if !foundA {
			idsA = []uint32{} // no A edges: nothing to follow
		}
		for _, mode := range []struct {
			name    string
			out, in bool
			types   map[string]bool
			ids     []uint32
		}{
			{"out", true, false, nil, nil},
			{"in", false, true, nil, nil},
			{"both", true, true, nil, nil},
			{"both:A", true, true, map[string]bool{"A": true}, idsA},
		} {
			for src := uint64(1); src <= nodes; src++ {
				for dst := uint64(1); dst <= nodes; dst++ {
					if src == dst {
						continue
					}
					for _, maxHops := range []int{-1, 2} {
						limit := nodes
						if maxHops > 0 {
							limit = maxHops
						}
						want := bruteShortest(edges, src, dst, mode.out, mode.in, mode.types, limit)
						var all, one []Path
						err := s.View(func(tx storage.Txn) error {
							spec := PathSpec{Out: mode.out, In: mode.in, MinHops: 1, MaxHops: maxHops, All: true}
							if mode.ids != nil {
								spec.Types = mode.ids
							}
							var err error
							if all, err = ShortestPaths(tx, src, dst, spec); err != nil {
								return err
							}
							spec.All = false
							one, err = ShortestPaths(tx, src, dst, spec)
							return err
						})
						if err != nil {
							t.Fatal(err)
						}
						ctx := fmt.Sprintf("round %d %s %d->%d max %d", round, mode.name, src, dst, maxHops)
						if got := pathIDs(all); strings.Join(got, ",") != strings.Join(want, ",") {
							t.Fatalf("%s: all = %v, want %v", ctx, got, want)
						}
						if (len(one) == 1) != (len(want) > 0) || len(one) > 1 {
							t.Fatalf("%s: one = %v, want one of %v", ctx, pathIDs(one), want)
						}
						if len(one) == 1 {
							checkWalk(t, ctx, one[0], src, dst, mode.out, mode.in)
						}
						for _, p := range all {
							checkWalk(t, ctx, p, src, dst, mode.out, mode.in)
						}
					}
				}
			}
		}
	}
}

func typeIDOf(s storage.Store, name string) (uint32, bool, error) {
	var (
		id    uint32
		found bool
	)
	err := s.View(func(tx storage.Txn) error {
		var err error
		id, found, err = catalog.LookupType(tx, name)
		return err
	})
	return id, found, err
}

// checkWalk verifies that consecutive nodes are joined by the listed edges.
func checkWalk(t *testing.T, ctx string, p Path, src, dst uint64, out, in bool) {
	t.Helper()
	if len(p.Nodes) != len(p.Rels)+1 || p.Nodes[0].ID != src || p.Nodes[len(p.Nodes)-1].ID != dst {
		t.Fatalf("%s: malformed path %+v", ctx, p)
	}
	for i, r := range p.Rels {
		a, b := p.Nodes[i].ID, p.Nodes[i+1].ID
		okOut := out && r.Src == a && r.Dst == b
		okIn := in && r.Dst == a && r.Src == b
		if !okOut && !okIn {
			t.Fatalf("%s: edge %d does not join %d and %d", ctx, r.ID, a, b)
		}
	}
}

func TestShortestPathsEdgeCases(t *testing.T) {
	s := newStore(t)
	var a, b uint64
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		if a, err = CreateNode(tx, nil, nil); err != nil {
			return err
		}
		if b, err = CreateNode(tx, nil, nil); err != nil {
			return err
		}
		_, err = CreateEdge(tx, "R", a, b, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.View(func(tx storage.Txn) error {
		if ps, _ := ShortestPaths(tx, a, a, PathSpec{Out: true, MinHops: 0, MaxHops: -1}); len(ps) != 1 || len(ps[0].Rels) != 0 {
			t.Errorf("a->a min 0 = %+v, want the empty path", ps)
		}
		if ps, _ := ShortestPaths(tx, a, a, PathSpec{Out: true, MinHops: 1, MaxHops: -1}); len(ps) != 0 {
			t.Errorf("a->a min 1 = %+v, want none", ps)
		}
		if ps, _ := ShortestPaths(tx, b, a, PathSpec{Out: true, MinHops: 1, MaxHops: -1}); len(ps) != 0 {
			t.Errorf("b->a against the direction = %+v, want none", ps)
		}
		if ps, _ := ShortestPaths(tx, a, b, PathSpec{Out: true, Types: []uint32{}, MaxHops: -1}); len(ps) != 0 {
			t.Errorf("with no existing type = %+v, want none", ps)
		}
		boom := fmt.Errorf("stop")
		if _, err := ShortestPaths(tx, a, b, PathSpec{Out: true, MaxHops: -1, Check: func() error { return boom }}); err != boom {
			t.Errorf("Check error = %v, want it returned", err)
		}
		return nil
	})
}
