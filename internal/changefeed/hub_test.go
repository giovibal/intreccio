package changefeed

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// fakeDict resolves dictionary IDs from fixed tables and counts lookups.
type fakeDict struct {
	mu      sync.Mutex
	labels  map[uint32]string
	types   map[uint32]string
	keys    map[uint32]string
	lookups int
	fail    bool
}

func (d *fakeDict) get(m map[uint32]string, id uint32) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lookups++
	if d.fail {
		return "", errors.New("dictionary unavailable")
	}
	name, ok := m[id]
	if !ok {
		return "", errors.New("unknown id")
	}
	return name, nil
}

func (d *fakeDict) LabelName(id uint32) (string, error) { return d.get(d.labels, id) }
func (d *fakeDict) TypeName(id uint32) (string, error)  { return d.get(d.types, id) }
func (d *fakeDict) KeyName(id uint32) (string, error)   { return d.get(d.keys, id) }

const (
	lPerson, lCompany, lOther uint32 = 1, 2, 3
	tKnows, tWorks            uint32 = 1, 2
	kName, kAge               uint32 = 1, 2
)

func newDict() *fakeDict {
	return &fakeDict{
		labels: map[uint32]string{lPerson: "Person", lCompany: "Company", lOther: "Other"},
		types:  map[uint32]string{tKnows: "KNOWS", tWorks: "WORKS_AT"},
		keys:   map[uint32]string{kName: "name", kAge: "age"},
	}
}

func set(names ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func node(labels []uint32, props map[uint32]any) *codec.NodeRecord {
	if props == nil {
		props = map[uint32]any{}
	}
	return &codec.NodeRecord{Labels: labels, Props: props}
}

func woken(w *Watcher) bool {
	select {
	case <-w.Dirty():
		return true
	default:
		return false
	}
}

func TestHubRelevance(t *testing.T) {
	person := []uint32{lPerson}
	nodes := func(before, after *codec.NodeRecord) *ChangeSet {
		return &ChangeSet{Nodes: []NodeChange{{ID: 1, Before: before, After: after}}}
	}
	edge := func(typ uint32, created bool) *ChangeSet {
		rec := &codec.EdgeRecord{Type: typ, Src: 1, Dst: 2, Props: map[uint32]any{}}
		if created {
			return &ChangeSet{Edges: []EdgeChange{{ID: 1, After: rec}}}
		}
		return &ChangeSet{Edges: []EdgeChange{{ID: 1, Before: rec}}}
	}

	personName := Footprint{Labels: set("Person"), Keys: set("name")}
	cases := []struct {
		name string
		fp   Footprint
		cs   *ChangeSet
		want bool
	}{
		{"create with a watched label", personName, nodes(nil, node(person, nil)), true},
		{"create with another label", personName, nodes(nil, node([]uint32{lCompany}, nil)), false},
		{"create without labels", personName, nodes(nil, node(nil, nil)), false},
		{"delete with a watched label", personName, nodes(node(person, nil), nil), true},
		{"watched key updated", personName,
			nodes(node(person, map[uint32]any{kName: "a"}), node(person, map[uint32]any{kName: "b"})), true},
		{"watched key added", personName,
			nodes(node(person, nil), node(person, map[uint32]any{kName: "b"})), true},
		{"watched key removed", personName,
			nodes(node(person, map[uint32]any{kName: "a"}), node(person, nil)), true},
		{"unwatched key updated", personName,
			nodes(node(person, map[uint32]any{kAge: int64(1)}), node(person, map[uint32]any{kAge: int64(2)})), false},
		{"unwatched key updated, any key", Footprint{Labels: set("Person"), AnyKey: true},
			nodes(node(person, map[uint32]any{kAge: int64(1)}), node(person, map[uint32]any{kAge: int64(2)})), true},
		{"watched key updated on another label", personName,
			nodes(node([]uint32{lCompany}, map[uint32]any{kName: "a"}), node([]uint32{lCompany}, map[uint32]any{kName: "b"})), false},
		{"watched label gained", personName, nodes(node(nil, nil), node(person, nil)), true},
		{"watched label lost", personName, nodes(node(person, nil), node(nil, nil)), true},
		{"other label gained on a watched node", personName,
			nodes(node(person, nil), node([]uint32{lPerson, lOther}, nil)), true},
		{"other label gained on an unwatched node", personName,
			nodes(node([]uint32{lCompany}, nil), node([]uint32{lCompany, lOther}, nil)), false},
		{"any label: create", Footprint{AnyLabel: true}, nodes(nil, node(nil, nil)), true},
		{"any label: unwatched key", Footprint{AnyLabel: true, Keys: set("name")},
			nodes(node(nil, map[uint32]any{kAge: int64(1)}), node(nil, map[uint32]any{kAge: int64(2)})), false},
		{"any label: watched key", Footprint{AnyLabel: true, Keys: set("name")},
			nodes(node(nil, map[uint32]any{kName: "a"}), node(nil, map[uint32]any{kName: "b"})), true},
		{"list value changed", personName,
			nodes(node(person, map[uint32]any{kName: []any{"a"}}), node(person, map[uint32]any{kName: []any{"b"}})), true},
		{"node change, relationships only", Footprint{Types: set("KNOWS")}, nodes(nil, node(person, nil)), false},
		{"watched type created", Footprint{Types: set("KNOWS")}, edge(tKnows, true), true},
		{"watched type deleted", Footprint{Types: set("KNOWS")}, edge(tKnows, false), true},
		{"other type created", Footprint{Types: set("KNOWS")}, edge(tWorks, true), false},
		{"any type", Footprint{AnyType: true}, edge(tWorks, true), true},
		{"relationship change, nodes only", personName, edge(tKnows, true), false},
		{"empty footprint", Footprint{}, nodes(nil, node(person, nil)), false},
		{"empty change set", Footprint{AnyLabel: true, AnyType: true, AnyKey: true}, &ChangeSet{}, false},
		{"unknown change", Footprint{}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewHub(newDict())
			w := h.Register(c.fp)
			defer h.Unregister(w)
			h.Committed(c.cs)
			if got := woken(w); got != c.want {
				t.Errorf("woken = %v, want %v", got, c.want)
			}
		})
	}
}

func TestHubWakesOnlyRelevantWatchers(t *testing.T) {
	h := NewHub(newDict())
	people := h.Register(Footprint{Labels: set("Person"), AnyKey: true})
	companies := h.Register(Footprint{Labels: set("Company"), AnyKey: true})
	knows := h.Register(Footprint{Labels: set("Person"), Types: set("KNOWS")})
	all := h.Register(Footprint{AnyLabel: true, AnyType: true, AnyKey: true})

	h.Committed(&ChangeSet{Nodes: []NodeChange{{ID: 1, After: node([]uint32{lCompany}, nil)}}})
	if woken(people) || !woken(companies) || woken(knows) || !woken(all) {
		t.Error("company created: wrong watchers woken")
	}

	h.Committed(&ChangeSet{Edges: []EdgeChange{{ID: 1, After: &codec.EdgeRecord{Type: tKnows}}}})
	if woken(people) || woken(companies) || !woken(knows) || !woken(all) {
		t.Error("KNOWS created: wrong watchers woken")
	}

	h.WakeAll()
	if !woken(people) || !woken(companies) || !woken(knows) || !woken(all) {
		t.Error("WakeAll: not every watcher woken")
	}
}

func TestHubCoalescesWakeUps(t *testing.T) {
	h := NewHub(newDict())
	w := h.Register(Footprint{AnyLabel: true})
	for range 5 {
		h.Committed(&ChangeSet{Nodes: []NodeChange{{ID: 1, After: node(nil, nil)}}})
	}
	if !woken(w) {
		t.Fatal("not woken")
	}
	if woken(w) {
		t.Error("several commits must coalesce into one pending wake-up")
	}
}

func TestHubActiveAndUnregister(t *testing.T) {
	h := NewHub(newDict())
	if h.Active() {
		t.Fatal("a new hub must be inactive")
	}
	h.Committed(nil) // no watchers: must be a no-op
	h.WakeAll()

	w := h.Register(Footprint{Labels: set("Person")})
	if !h.Active() {
		t.Fatal("hub with a watcher must be active")
	}
	h.Unregister(w)
	h.Unregister(w) // idempotent
	if h.Active() {
		t.Fatal("hub must be inactive once its only watcher is gone")
	}
	h.Committed(&ChangeSet{Nodes: []NodeChange{{ID: 1, After: node([]uint32{lPerson}, nil)}}})
	if woken(w) {
		t.Error("an unregistered watcher must not be woken")
	}
}

func TestHubCachesNamesAndFailsOpen(t *testing.T) {
	d := newDict()
	h := NewHub(d)
	w := h.Register(Footprint{Labels: set("Company")})
	cs := &ChangeSet{Nodes: []NodeChange{{ID: 1, After: node([]uint32{lPerson}, nil)}}}

	h.Committed(cs)
	h.Committed(cs)
	if d.lookups != 1 {
		t.Errorf("dictionary lookups = %d, want 1 (names are cached)", d.lookups)
	}
	if woken(w) {
		t.Fatal("an irrelevant change woke the watcher")
	}

	// When a name cannot be resolved the hub cannot filter: it wakes everyone.
	d.fail = true
	h.Committed(&ChangeSet{Nodes: []NodeChange{{ID: 2, After: node([]uint32{lOther}, nil)}}})
	if !woken(w) {
		t.Error("an unresolvable change must wake every watcher")
	}
}

func TestHubConcurrentUse(t *testing.T) {
	h := NewHub(newDict())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				w := h.Register(Footprint{Labels: set("Person"), AnyKey: i%2 == 0})
				h.Committed(&ChangeSet{Nodes: []NodeChange{{ID: 1, After: node([]uint32{lPerson}, nil)}}})
				h.WakeAll()
				h.Unregister(w)
			}
		}()
	}
	wg.Wait()
	if h.Active() {
		t.Error("watchers leaked")
	}
}

func TestHubInvalidate(t *testing.T) {
	d := newDict()
	h := NewHub(d)
	w := h.Register(Footprint{Labels: set("Company")})
	cs := &ChangeSet{Nodes: []NodeChange{{ID: 1, After: node([]uint32{lPerson}, nil)}}}
	h.Committed(cs)
	if woken(w) {
		t.Fatal("an irrelevant change woke the watcher")
	}

	// After a restore the same ID may name something else.
	d.mu.Lock()
	d.labels[lPerson] = "Company"
	d.mu.Unlock()
	h.Invalidate()
	if !woken(w) {
		t.Error("Invalidate must wake every watcher")
	}
	h.Committed(cs)
	if !woken(w) {
		t.Error("Invalidate must drop cached names")
	}
}

func takeDirty(t *testing.T, w *Watcher) ([]uint64, bool) {
	t.Helper()
	ids, full := w.TakeDirty()
	return ids, full
}

func TestHubTracksDirtyNodes(t *testing.T) {
	h := NewHub(newDict())
	w := h.RegisterTracking(Footprint{Labels: set("Person"), Types: set("KNOWS"), Keys: set("name")})
	plain := h.Register(Footprint{AnyLabel: true, AnyType: true, AnyKey: true})
	person := []uint32{lPerson}

	if ids, full := takeDirty(t, w); len(ids) != 0 || full {
		t.Fatalf("fresh watcher: dirty = %v, full = %v", ids, full)
	}

	// Relevant node changes are recorded, irrelevant ones are not.
	h.Committed(&ChangeSet{Nodes: []NodeChange{
		{ID: 9, After: node(person, nil)},
		{ID: 3, Before: node(person, map[uint32]any{kName: "a"}), After: node(person, map[uint32]any{kName: "b"})},
		{ID: 5, After: node([]uint32{lCompany}, nil)},                                                                      // other label
		{ID: 6, Before: node(person, map[uint32]any{kAge: int64(1)}), After: node(person, map[uint32]any{kAge: int64(2)})}, // unwatched key
	}})
	if !woken(w) {
		t.Fatal("not woken")
	}
	if ids, full := takeDirty(t, w); !reflect.DeepEqual(ids, []uint64{3, 9}) || full {
		t.Errorf("dirty = %v (full %v), want [3 9]", ids, full)
	}
	if ids, _ := takeDirty(t, w); len(ids) != 0 {
		t.Errorf("TakeDirty did not clear: %v", ids)
	}

	// A relationship change dirties both of its ends, created or deleted.
	h.Committed(&ChangeSet{Edges: []EdgeChange{
		{ID: 1, After: &codec.EdgeRecord{Type: tKnows, Src: 20, Dst: 21}},
		{ID: 2, Before: &codec.EdgeRecord{Type: tKnows, Src: 22, Dst: 20}},
		{ID: 3, After: &codec.EdgeRecord{Type: tWorks, Src: 30, Dst: 31}}, // other type
	}})
	if ids, full := takeDirty(t, w); !reflect.DeepEqual(ids, []uint64{20, 21, 22}) || full {
		t.Errorf("dirty after relationship changes = %v (full %v), want [20 21 22]", ids, full)
	}

	// Anything the hub cannot describe asks for a full re-evaluation.
	for name, unknown := range map[string]func(){
		"WakeAll":        h.WakeAll,
		"Committed(nil)": func() { h.Committed(nil) },
		"Invalidate":     h.Invalidate,
	} {
		woken(w)
		unknown()
		if ids, full := takeDirty(t, w); !full || len(ids) != 0 {
			t.Errorf("%s: dirty = %v, full = %v; want a full re-evaluation", name, ids, full)
		}
		if _, full := takeDirty(t, w); full {
			t.Errorf("%s: TakeDirty did not clear the full flag", name)
		}
	}

	// So does a dirty set too large to be worth tracking.
	var many []NodeChange
	for i := range maxDirtyNodes + 1 {
		many = append(many, NodeChange{ID: uint64(1000 + i), After: node(person, nil)})
	}
	h.Committed(&ChangeSet{Nodes: many})
	if ids, full := takeDirty(t, w); !full || len(ids) != 0 {
		t.Errorf("overflow: %d dirty nodes, full = %v; want a full re-evaluation", len(ids), full)
	}

	// Remark puts work back and wakes.
	woken(w)
	w.Remark([]uint64{7, 8}, false)
	if !woken(w) {
		t.Error("Remark did not wake")
	}
	if ids, full := takeDirty(t, w); !reflect.DeepEqual(ids, []uint64{7, 8}) || full {
		t.Errorf("after Remark: dirty = %v (full %v)", ids, full)
	}
	w.Remark(nil, true)
	if _, full := takeDirty(t, w); !full {
		t.Error("Remark(full) lost the flag")
	}

	// A plain watcher is woken but records nothing.
	if ids, full := takeDirty(t, plain); len(ids) != 0 || full {
		t.Errorf("plain watcher: dirty = %v, full = %v", ids, full)
	}
}

// TestHubAwaitReported checks the handshake that lets an evaluation know every
// commit its snapshot may contain has been reported.
func TestHubAwaitReported(t *testing.T) {
	s := newStore(t)
	h := NewHub(newDict())

	if err := h.AwaitReported(context.Background(), h.Started()); err != nil {
		t.Fatalf("nothing in flight: %v", err)
	}

	// A write in flight holds back whoever waits for its sequence number.
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- h.Update(s, func(storage.Txn) error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	seq := h.Started()
	if seq == 0 {
		t.Fatal("Started did not count the write in flight")
	}

	waited := make(chan error, 1)
	go func() { waited <- h.AwaitReported(context.Background(), seq) }()
	select {
	case err := <-waited:
		t.Fatalf("AwaitReported returned (%v) while the write was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Waiting for an earlier point is not held back by a later write.
	if err := h.AwaitReported(context.Background(), seq-1); err != nil {
		t.Fatalf("AwaitReported(seq-1): %v", err)
	}
	// And a waiter gives up with its context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.AwaitReported(ctx, seq); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitReported with an expiring context: %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Update: %v", err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("AwaitReported: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AwaitReported never returned after the write reported")
	}

	// A failed write is no longer in flight either.
	_ = h.Update(s, func(storage.Txn) error { return errors.New("boom") })
	if err := h.AwaitReported(context.Background(), h.Started()); err != nil {
		t.Fatalf("after a failed write: %v", err)
	}
}
