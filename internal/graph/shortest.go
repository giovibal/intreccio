package graph

import "github.com/giovibal/intreccio/internal/storage"

// PathSpec configures ShortestPaths.
type PathSpec struct {
	// Out / In: follow outgoing / incoming edges (both for an undirected
	// pattern).
	Out, In bool
	// Types restricts the relationship types; nil means any type.
	Types []uint32
	// MinHops is 0 or 1. With 0, a node is joined to itself by the empty path;
	// with 1 it is not (and no cycle is searched for).
	MinHops int
	// MaxHops bounds the path length; a negative value means unbounded.
	MaxHops int
	// All returns every shortest path instead of one.
	All bool
	// Check, if set, is called regularly; an error aborts the search (used
	// for context cancellation).
	Check func() error
}

// Path is a walk from Nodes[0]: Rels[i] joins Nodes[i] and Nodes[i+1].
// ShortestPaths fills only the IDs (and the edge type/endpoints); records are
// loaded by the caller as needed.
type Path struct {
	Nodes []Node
	Rels  []Edge
}

// pred is how a BFS reached a node: from node prev through edge.
type pred struct {
	prev uint64
	edge EdgeRef
}

// bfsSide is one direction of the bidirectional search.
type bfsSide struct {
	depth    map[uint64]int
	preds    map[uint64][]pred
	frontier []uint64
	level    int
	out, in  bool // edges to follow from this side
}

func newSide(start uint64, out, in bool) *bfsSide {
	return &bfsSide{
		depth:    map[uint64]int{start: 0},
		preds:    map[uint64][]pred{},
		frontier: []uint64{start},
		out:      out,
		in:       in,
	}
}

// ShortestPaths returns the shortest path(s) from src to dst under spec, or
// none. It runs a bidirectional breadth-first search over the adjacency
// entries, one full layer at a time from the smaller frontier, so it visits
// about the square root of what a one-sided search would on a branching
// graph. A shortest path never repeats a relationship, so the result follows
// trail semantics.
func ShortestPaths(txn storage.Txn, src, dst uint64, spec PathSpec) ([]Path, error) {
	if src == dst {
		if spec.MinHops == 0 {
			return []Path{{Nodes: []Node{{ID: src}}}}, nil
		}
		return nil, nil
	}
	if spec.MaxHops == 0 || (spec.Types != nil && len(spec.Types) == 0) {
		return nil, nil
	}
	fwd := newSide(src, spec.Out, spec.In)
	bwd := newSide(dst, spec.In, spec.Out) // walks edges backwards
	for len(fwd.frontier) > 0 && len(bwd.frontier) > 0 {
		if spec.MaxHops > 0 && fwd.level+bwd.level+1 > spec.MaxHops {
			return nil, nil
		}
		side, other := fwd, bwd
		if len(bwd.frontier) < len(fwd.frontier) {
			side, other = bwd, fwd
		}
		meet, err := side.expand(txn, other, spec)
		if err != nil {
			return nil, err
		}
		if len(meet) > 0 {
			return joinPaths(fwd, bwd, meet, spec.All), nil
		}
	}
	return nil, nil
}

// expand advances s by one layer and returns the newly reached nodes that the
// other side has already reached, at the smallest combined distance.
func (s *bfsSide) expand(txn storage.Txn, other *bfsSide, spec PathSpec) ([]uint64, error) {
	s.level++
	var next []uint64
	for _, n := range s.frontier {
		if spec.Check != nil {
			if err := spec.Check(); err != nil {
				return nil, err
			}
		}
		edges, err := neighbors(txn, n, s.out, s.in, spec.Types)
		if err != nil {
			return nil, err
		}
		for _, e := range edges {
			m := e.Src
			if m == n {
				m = e.Dst
			}
			if m == n {
				continue // self-loop: never on a shortest path
			}
			d, seen := s.depth[m]
			switch {
			case !seen:
				s.depth[m] = s.level
				next = append(next, m)
			case d != s.level:
				continue // reached earlier by a shorter route
			}
			s.preds[m] = append(s.preds[m], pred{prev: n, edge: e})
		}
	}
	s.frontier = next
	best := -1
	var meet []uint64
	for _, m := range next {
		od, ok := other.depth[m]
		if !ok {
			continue
		}
		switch {
		case best < 0 || od < best:
			best, meet = od, []uint64{m}
		case od == best:
			meet = append(meet, m)
		}
	}
	return meet, nil
}

// joinPaths combines, for every meeting node, the walks from src to it with
// the walks from it to dst. With all false it stops at the first path.
func joinPaths(fwd, bwd *bfsSide, meet []uint64, all bool) []Path {
	var out []Path
	for _, m := range meet {
		heads := walks(fwd, m, !all)
		tails := walks(bwd, m, !all)
		for _, h := range heads {
			for _, t := range tails {
				out = append(out, concat(h, t))
				if !all {
					return out
				}
			}
		}
	}
	return out
}

// walks lists the walks from the side's start to m as alternating node IDs
// and edges, start first. one keeps only the first predecessor at each step.
func walks(s *bfsSide, m uint64, one bool) []Path {
	ps := s.preds[m]
	if len(ps) == 0 { // m is the start
		return []Path{{Nodes: []Node{{ID: m}}}}
	}
	if one {
		ps = ps[:1]
	}
	var out []Path
	for _, p := range ps {
		for _, w := range walks(s, p.prev, one) {
			w.Nodes = append(w.Nodes, Node{ID: m})
			w.Rels = append(w.Rels, edgeOf(p.edge))
			out = append(out, w)
		}
	}
	return out
}

// concat joins a walk src→m with a walk dst→m (reversed into m→dst).
func concat(head, tail Path) Path {
	p := Path{
		Nodes: append([]Node(nil), head.Nodes...),
		Rels:  append([]Edge(nil), head.Rels...),
	}
	for i := len(tail.Rels) - 1; i >= 0; i-- {
		p.Rels = append(p.Rels, tail.Rels[i])
		p.Nodes = append(p.Nodes, tail.Nodes[i])
	}
	return p
}

func edgeOf(r EdgeRef) Edge {
	e := Edge{ID: r.ID}
	e.Type, e.Src, e.Dst = r.Type, r.Src, r.Dst
	return e
}

// neighbors lists the edges of n to follow: outgoing and/or incoming, of the
// given types (nil = any).
func neighbors(txn storage.Txn, n uint64, out, in bool, types []uint32) ([]EdgeRef, error) {
	var res []EdgeRef
	scan := func(f func(storage.Txn, uint64, uint32) ([]EdgeRef, error)) error {
		if types == nil {
			es, err := f(txn, n, 0)
			res = append(res, es...)
			return err
		}
		for _, t := range types {
			es, err := f(txn, n, t)
			if err != nil {
				return err
			}
			res = append(res, es...)
		}
		return nil
	}
	if out {
		if err := scan(OutEdges); err != nil {
			return nil, err
		}
	}
	if in {
		if err := scan(InEdges); err != nil {
			return nil, err
		}
	}
	return res, nil
}
