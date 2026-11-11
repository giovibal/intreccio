package changefeed

import (
	"errors"
	"sync"
	"testing"

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
