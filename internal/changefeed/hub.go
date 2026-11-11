package changefeed

import (
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// Footprint is the part of the graph a query can depend on, as names. It is a
// conservative summary: it may wake a watcher needlessly, never miss a change.
//
//   - A node that is created, deleted or relabelled concerns the query when it
//     carries, before or after, one of Labels (or AnyLabel is set).
//   - A node whose properties alone changed concerns it when, in addition, one
//     of the changed keys is in Keys (or AnyKey is set).
//   - A relationship that is created or deleted concerns it when its type is in
//     Types (or AnyType is set).
//
// The zero Footprint depends on nothing in the graph.
type Footprint struct {
	Labels   map[string]struct{}
	Types    map[string]struct{}
	Keys     map[string]struct{}
	AnyLabel bool
	AnyType  bool
	AnyKey   bool
}

// Resolver maps dictionary IDs to names. A ChangeSet carries IDs, footprints
// carry names: a label a query mentions may not exist yet when it subscribes.
type Resolver interface {
	LabelName(id uint32) (string, error)
	TypeName(id uint32) (string, error)
	KeyName(id uint32) (string, error)
}

// Watcher is one registered live query. It only ever carries a pending wake-up:
// any number of relevant commits collapse into one.
type Watcher struct {
	fp    Footprint
	dirty chan struct{}
}

// Dirty delivers a value when a commit that may concern the watcher has
// happened since the last receive.
func (w *Watcher) Dirty() <-chan struct{} { return w.dirty }

func (w *Watcher) wake() {
	select {
	case w.dirty <- struct{}{}:
	default: // already pending
	}
}

type watcherSet map[*Watcher]struct{}

// Hub is the in-memory registry of watchers. Commit points report to it and it
// wakes the watchers a commit may concern. It is safe for concurrent use and
// never blocks on a watcher.
type Hub struct {
	resolver Resolver
	active   atomic.Int64 // number of registered watchers

	mu       sync.RWMutex
	watchers watcherSet
	byLabel  map[string]watcherSet
	byType   map[string]watcherSet
	anyLabel watcherSet
	anyType  watcherSet

	// Dictionary entries are immutable once allocated, so names are cached.
	nameMu sync.Mutex
	labels map[uint32]string
	types  map[uint32]string
	keys   map[uint32]string
}

// NewHub returns an empty hub resolving dictionary IDs through r.
func NewHub(r Resolver) *Hub {
	return &Hub{
		resolver: r,
		watchers: watcherSet{},
		byLabel:  map[string]watcherSet{},
		byType:   map[string]watcherSet{},
		anyLabel: watcherSet{},
		anyType:  watcherSet{},
		labels:   map[uint32]string{},
		types:    map[uint32]string{},
		keys:     map[uint32]string{},
	}
}

// Active reports whether any watcher is registered. Commit points use it to
// skip the capture entirely when nobody is listening.
func (h *Hub) Active() bool { return h.active.Load() > 0 }

// Register adds a watcher for fp. Register it before running the query's
// initial evaluation, so that no commit can fall between the two.
func (h *Hub) Register(fp Footprint) *Watcher {
	w := &Watcher{fp: fp, dirty: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watchers[w] = struct{}{}
	if fp.AnyLabel {
		h.anyLabel[w] = struct{}{}
	}
	for l := range fp.Labels {
		addTo(h.byLabel, l, w)
	}
	if fp.AnyType {
		h.anyType[w] = struct{}{}
	}
	for t := range fp.Types {
		addTo(h.byType, t, w)
	}
	h.active.Add(1)
	return w
}

// Unregister removes w. It is idempotent.
func (h *Hub) Unregister(w *Watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.watchers[w]; !ok {
		return
	}
	delete(h.watchers, w)
	delete(h.anyLabel, w)
	delete(h.anyType, w)
	for l := range w.fp.Labels {
		removeFrom(h.byLabel, l, w)
	}
	for t := range w.fp.Types {
		removeFrom(h.byType, t, w)
	}
	h.active.Add(-1)
}

func addTo(index map[string]watcherSet, name string, w *Watcher) {
	set := index[name]
	if set == nil {
		set = watcherSet{}
		index[name] = set
	}
	set[w] = struct{}{}
}

func removeFrom(index map[string]watcherSet, name string, w *Watcher) {
	set := index[name]
	delete(set, w)
	if len(set) == 0 {
		delete(index, name)
	}
}

// Update is how a commit point runs a write transaction: it executes fn through
// store.Update and, once the transaction has committed, reports its effect to
// the hub. It is the single implementation of the commit-reporting rule:
//
//   - a capture is installed only while a watcher exists, so a write nobody
//     watches pays one atomic load;
//   - every attempt starts from a fresh capture, because store.Update may run
//     fn again on a conflict;
//   - nothing is reported unless the commit succeeded;
//   - a commit that cannot be described — it ran without a capture, or its
//     ChangeSet could not be built — is reported as unknown and wakes every
//     watcher. That also covers a watcher registering while a write that began
//     without a capture is still in flight;
//   - describing the write never fails it.
func (h *Hub) Update(store storage.Store, fn func(storage.Txn) error) error {
	var changes *ChangeSet
	err := store.Update(func(txn storage.Txn) error {
		changes = nil
		var capture *Capture
		if h.Active() {
			capture = NewCapture(txn)
			txn = capture
		}
		if err := fn(txn); err != nil {
			return err
		}
		if capture != nil {
			if cs, err := capture.ChangeSet(); err == nil {
				changes = cs
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.Committed(changes)
	return nil
}

// Invalidate reports that the whole state was replaced without going through a
// write transaction (a snapshot restore). Cached dictionary names are dropped
// and every watcher is woken; each one's next evaluation yields the net
// difference.
func (h *Hub) Invalidate() {
	h.nameMu.Lock()
	h.labels = map[uint32]string{}
	h.types = map[uint32]string{}
	h.keys = map[uint32]string{}
	h.nameMu.Unlock()
	h.WakeAll()
}

// WakeAll wakes every watcher. It is the answer to any state change the hub
// cannot describe.
func (h *Hub) WakeAll() {
	if !h.Active() {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for w := range h.watchers {
		w.wake()
	}
}

// Committed reports a successfully committed write transaction. A nil cs means
// the effect is unknown — the transaction ran without a capture, or its
// ChangeSet could not be built — and wakes every watcher.
func (h *Hub) Committed(cs *ChangeSet) {
	if !h.Active() {
		return
	}
	if cs == nil {
		h.WakeAll()
		return
	}
	if cs.Empty() {
		return
	}
	nodes, types, err := h.describe(cs)
	if err != nil {
		h.WakeAll() // cannot filter without names: fail open
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, n := range nodes {
		for w := range h.anyLabel {
			if n.concerns(&w.fp) {
				w.wake()
			}
		}
		for _, l := range n.labels {
			for w := range h.byLabel[l] {
				if n.concerns(&w.fp) {
					w.wake()
				}
			}
		}
	}
	for _, t := range types {
		for w := range h.anyType {
			w.wake()
		}
		for w := range h.byType[t] {
			w.wake()
		}
	}
}

// nodeEvent is a NodeChange with names resolved, ready to be matched against
// footprints. The index lookup has already established the label condition.
type nodeEvent struct {
	labels     []string // every label the node carries before or after
	structural bool     // created, deleted or relabelled
	keys       []string // changed property keys, when not structural
}

func (n *nodeEvent) concerns(fp *Footprint) bool {
	if n.structural || fp.AnyKey {
		return true
	}
	for _, k := range n.keys {
		if _, ok := fp.Keys[k]; ok {
			return true
		}
	}
	return false
}

// describe resolves a ChangeSet into node events and the relationship types it
// touches.
func (h *Hub) describe(cs *ChangeSet) ([]nodeEvent, []string, error) {
	h.nameMu.Lock()
	defer h.nameMu.Unlock()

	nodes := make([]nodeEvent, 0, len(cs.Nodes))
	for _, c := range cs.Nodes {
		var ev nodeEvent
		seen := map[uint32]struct{}{}
		for _, rec := range []*codec.NodeRecord{c.Before, c.After} {
			if rec == nil {
				continue
			}
			for _, id := range rec.Labels {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				name, err := lookup(h.labels, h.resolver.LabelName, id)
				if err != nil {
					return nil, nil, err
				}
				ev.labels = append(ev.labels, name)
			}
		}
		if c.Before == nil || c.After == nil || !sameLabels(c.Before.Labels, c.After.Labels) {
			ev.structural = true
		} else {
			for _, id := range changedKeys(c.Before.Props, c.After.Props) {
				name, err := lookup(h.keys, h.resolver.KeyName, id)
				if err != nil {
					return nil, nil, err
				}
				ev.keys = append(ev.keys, name)
			}
		}
		nodes = append(nodes, ev)
	}

	var types []string
	seen := map[uint32]struct{}{}
	for _, c := range cs.Edges {
		for _, rec := range []*codec.EdgeRecord{c.Before, c.After} {
			if rec == nil {
				continue
			}
			if _, dup := seen[rec.Type]; dup {
				continue
			}
			seen[rec.Type] = struct{}{}
			name, err := lookup(h.types, h.resolver.TypeName, rec.Type)
			if err != nil {
				return nil, nil, err
			}
			types = append(types, name)
		}
	}
	return nodes, types, nil
}

func lookup(cache map[uint32]string, resolve func(uint32) (string, error), id uint32) (string, error) {
	if name, ok := cache[id]; ok {
		return name, nil
	}
	name, err := resolve(id)
	if err != nil {
		return "", err
	}
	cache[id] = name
	return name, nil
}

func sameLabels(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[uint32]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; !ok {
			return false
		}
	}
	return true
}

// changedKeys lists the property keys whose value differs between a and b,
// including keys present on one side only.
func changedKeys(a, b map[uint32]any) []uint32 {
	var out []uint32
	for k, av := range a {
		if bv, ok := b[k]; !ok || !reflect.DeepEqual(av, bv) {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}
