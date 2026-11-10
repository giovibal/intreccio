// Package overlay buffers the writes of a read-write transaction for the
// storage adapters: an ordered map from keys to values or deletion markers,
// and an iterator that merges it with the engine's committed data.
//
// Snapshots are cheap: Snapshot freezes the nodes that exist, and later
// writes copy the O(log n) frozen nodes on their path instead of changing
// them, so a snapshot keeps reading the map as it was. Writes made when no
// snapshot has been taken since change nodes in place. An iterator works on
// a snapshot, so it sees the writes made before it was created and none made
// while it is open, the semantics the upper layers rely on (DESIGN §7).
// Opening one costs O(log n), not a copy or sort of every buffered write.
package overlay

import (
	"bytes"
	"hash/maphash"
)

// Tree is an ordered map; the zero value is empty. It is not safe for
// concurrent use.
type Tree struct {
	root  *node
	n     int
	epoch uint64 // nodes created in an older epoch are frozen
}

// Snapshot is a read-only view of a Tree at one point in time.
type Snapshot struct {
	root *node
}

// node is a treap node: ordered by key, and a max-heap on prio, which is a
// hash of the key so the shape (and so the cost) does not depend on the order
// keys are inserted in.
type node struct {
	key         []byte
	val         []byte
	deleted     bool
	prio        uint64
	epoch       uint64
	left, right *node
}

var seed = maphash.MakeSeed()

// Len reports the number of keys, deletion markers included.
func (t *Tree) Len() int { return t.n }

// Snapshot freezes the current contents and returns a view of them.
func (t *Tree) Snapshot() Snapshot {
	t.epoch++
	return Snapshot{root: t.root}
}

// Get returns the value buffered for key. found reports whether the key has
// an entry; deleted whether that entry is a deletion marker.
func (t *Tree) Get(key []byte) (val []byte, deleted, found bool) {
	return Snapshot{root: t.root}.Get(key)
}

// Get is Tree.Get on the snapshot.
func (s Snapshot) Get(key []byte) (val []byte, deleted, found bool) {
	n := s.root
	for n != nil {
		switch c := bytes.Compare(key, n.key); {
		case c < 0:
			n = n.left
		case c > 0:
			n = n.right
		default:
			return n.val, n.deleted, true
		}
	}
	return nil, false, false
}

// Put sets key to val. It keeps both slices: the caller must not modify
// them afterwards.
func (t *Tree) Put(key, val []byte) { t.set(key, val, false) }

// Delete sets a deletion marker for key.
func (t *Tree) Delete(key []byte) { t.set(key, nil, true) }

func (t *Tree) set(key, val []byte, deleted bool) {
	added := false
	t.root = t.insert(t.root, key, val, deleted, maphash.Bytes(seed, key), &added)
	if added {
		t.n++
	}
}

// own returns n itself when it belongs to the current epoch, or a copy of
// it when a snapshot may still read it.
func (t *Tree) own(n *node) *node {
	if n.epoch == t.epoch {
		return n
	}
	cp := *n
	cp.epoch = t.epoch
	return &cp
}

// insert returns the subtree n with key set. Frozen nodes on the path are
// copied, never changed; nodes the rotations touch are already owned.
func (t *Tree) insert(n *node, key, val []byte, deleted bool, prio uint64, added *bool) *node {
	if n == nil {
		*added = true
		return &node{key: key, val: val, deleted: deleted, prio: prio, epoch: t.epoch}
	}
	switch c := bytes.Compare(key, n.key); {
	case c == 0:
		n = t.own(n)
		n.val, n.deleted = val, deleted
	case c < 0:
		left := t.insert(n.left, key, val, deleted, prio, added)
		n = t.own(n)
		n.left = left
		if left.prio > n.prio { // rotate right
			n.left = left.right
			left.right = n
			return left
		}
	default:
		right := t.insert(n.right, key, val, deleted, prio, added)
		n = t.own(n)
		n.right = right
		if right.prio > n.prio { // rotate left
			n.right = right.left
			right.left = n
			return right
		}
	}
	return n
}

// Iter walks the entries of a Tree in key order.
type Iter struct {
	stack []*node // path to the current node; the top is the current entry
}

// Seek returns an iterator over a snapshot of the tree, positioned at the
// first key >= start (all keys when start is nil).
func (t *Tree) Seek(start []byte) *Iter { return t.Snapshot().Seek(start) }

// Seek returns an iterator positioned at the first key >= start.
func (s Snapshot) Seek(start []byte) *Iter {
	it := &Iter{}
	for n := s.root; n != nil; {
		if bytes.Compare(n.key, start) >= 0 {
			it.stack = append(it.stack, n)
			n = n.left
		} else {
			n = n.right
		}
	}
	return it
}

// Valid reports whether the iterator is on an entry.
func (it *Iter) Valid() bool { return len(it.stack) > 0 }

// Key returns the current key (owned by the Tree: do not modify).
func (it *Iter) Key() []byte { return it.stack[len(it.stack)-1].key }

// Value returns the current value and whether it is a deletion marker.
func (it *Iter) Value() (val []byte, deleted bool) {
	n := it.stack[len(it.stack)-1]
	return n.val, n.deleted
}

// Next moves to the following key.
func (it *Iter) Next() {
	n := it.stack[len(it.stack)-1]
	it.stack = it.stack[:len(it.stack)-1]
	for m := n.right; m != nil; m = m.left {
		it.stack = append(it.stack, m)
	}
}

// Source is the engine side of a Merged iterator: the committed keys under
// a prefix, in order, starting at the scan's start key.
type Source interface {
	Valid() bool
	// Key returns the current key; Merged copies it before handing it out.
	Key() []byte
	// Value returns a copy of the current value.
	Value() ([]byte, error)
	Next()
	Close() error
}

// Merged iterates a Source together with a Tree snapshot, within prefix: a
// buffered entry wins over the committed one with the same key, and deletion
// markers hide keys. It implements storage.Iterator.
type Merged struct {
	base   Source
	ov     *Iter
	prefix []byte
	fromOv bool // the current entry comes from the overlay
	valid  bool
}

// NewMerged starts a merged scan. ov must be positioned at the same start
// key as base.
func NewMerged(base Source, ov *Iter, prefix []byte) *Merged {
	m := &Merged{base: base, ov: ov, prefix: prefix}
	m.settle()
	return m
}

func (m *Merged) ovKey() []byte {
	if !m.ov.Valid() || !bytes.HasPrefix(m.ov.Key(), m.prefix) {
		return nil
	}
	return m.ov.Key()
}

// settle positions on the next visible entry of either side.
func (m *Merged) settle() {
	for {
		var bk []byte
		if m.base.Valid() {
			bk = m.base.Key()
		}
		ok := m.ovKey()
		switch {
		case bk == nil && ok == nil:
			m.valid = false
			return
		case ok == nil || (bk != nil && bytes.Compare(bk, ok) < 0):
			m.valid, m.fromOv = true, false
			return
		}
		if bk != nil && bytes.Equal(bk, ok) {
			m.base.Next() // shadowed by the buffered entry
		}
		if _, del := m.ov.Value(); del {
			m.ov.Next()
			continue
		}
		m.valid, m.fromOv = true, true
		return
	}
}

// Valid reports whether the iterator is on an entry.
func (m *Merged) Valid() bool { return m.valid }

// Next moves to the next entry.
func (m *Merged) Next() {
	if !m.valid {
		return
	}
	if m.fromOv {
		m.ov.Next()
	} else {
		m.base.Next()
	}
	m.settle()
}

// Key returns a copy of the current key.
func (m *Merged) Key() []byte {
	if m.fromOv {
		return bytes.Clone(m.ov.Key())
	}
	return bytes.Clone(m.base.Key())
}

// Value returns a copy of the current value.
func (m *Merged) Value() ([]byte, error) {
	if m.fromOv {
		v, _ := m.ov.Value()
		out := make([]byte, len(v)) // non-nil even when empty
		copy(out, v)
		return out, nil
	}
	return m.base.Value()
}

// Close closes the engine side.
func (m *Merged) Close() error {
	m.valid = false
	return m.base.Close()
}
