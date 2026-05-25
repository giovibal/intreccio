package catalog

import (
	"sync"
	"testing"

	"github.com/giovibal/mycypher/internal/storage"
	badgeradapter "github.com/giovibal/mycypher/internal/storage/badger"
)

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := badgeradapter.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInternIdempotentAndReverse(t *testing.T) {
	s := newStore(t)

	var alice, bob, alice2 uint32
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		if alice, err = InternLabel(tx, "Person"); err != nil {
			return err
		}
		if bob, err = InternLabel(tx, "Company"); err != nil {
			return err
		}
		alice2, err = InternLabel(tx, "Person")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if alice != alice2 {
		t.Errorf("intern non idempotente: %d != %d", alice, alice2)
	}
	if alice == bob {
		t.Error("label distinte devono avere ID distinti")
	}
	if alice == 0 || bob == 0 {
		t.Error("gli ID devono partire da 1 (0 riservato)")
	}

	// I tre dizionari sono indipendenti: stesso nome → ID allocati separatamente.
	if err := s.Update(func(tx storage.Txn) error {
		lab, _ := InternLabel(tx, "Same")
		typ, _ := InternType(tx, "Same")
		key, _ := InternKey(tx, "Same")
		if lab == 0 || typ == 0 || key == 0 {
			t.Error("ID non validi")
		}
		// reverse
		if n, err := LabelName(tx, alice); err != nil || n != "Person" {
			t.Errorf("LabelName(%d) = (%q, %v)", alice, n, err)
		}
		if n, err := TypeName(tx, typ); err != nil || n != "Same" {
			t.Errorf("TypeName = (%q, %v)", n, err)
		}
		if n, err := KeyName(tx, key); err != nil || n != "Same" {
			t.Errorf("KeyName = (%q, %v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCountersMonotonic(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		n1, _ := NextNodeID(tx)
		n2, _ := NextNodeID(tx)
		e1, _ := NextEdgeID(tx)
		if n1 != 1 || n2 != 2 {
			t.Errorf("node id non monotòni: %d, %d", n1, n2)
		}
		if e1 != 1 {
			t.Errorf("edge id deve partire da 1, got %d", e1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexRegistry(t *testing.T) {
	s := newStore(t)
	if err := s.Update(func(tx storage.Txn) error {
		if has, _ := HasIndex(tx, 1, 2); has {
			t.Error("indice non dovrebbe esistere")
		}
		if err := AddIndex(tx, 1, 2); err != nil {
			return err
		}
		if err := AddIndex(tx, 3, 4); err != nil {
			return err
		}
		if err := AddIndex(tx, 1, 2); err != nil { // idempotente
			return err
		}
		if has, _ := HasIndex(tx, 1, 2); !has {
			t.Error("indice (1,2) dovrebbe esistere")
		}
		defs, err := ListIndexes(tx)
		if err != nil {
			return err
		}
		if len(defs) != 2 {
			t.Errorf("attesi 2 indici, got %d: %v", len(defs), defs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Intern concorrente dello stesso insieme di nomi: nessun ID duplicato, ID stabili.
func TestInternConcurrent(t *testing.T) {
	s := newStore(t)
	names := []string{"A", "B", "C", "D", "E"}
	const workers = 16

	results := make([]map[string]uint32, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			m := make(map[string]uint32, len(names))
			for _, name := range names {
				err := s.Update(func(tx storage.Txn) error {
					id, err := InternLabel(tx, name)
					if err != nil {
						return err
					}
					m[name] = id
					return nil
				})
				if err != nil {
					t.Errorf("worker %d: %v", w, err)
				}
			}
			results[w] = m
		}(w)
	}
	wg.Wait()

	// Tutti i worker devono vedere gli stessi ID per gli stessi nomi.
	ref := results[0]
	for w := 1; w < workers; w++ {
		for _, name := range names {
			if results[w][name] != ref[name] {
				t.Errorf("ID incoerente per %q: worker0=%d worker%d=%d", name, ref[name], w, results[w][name])
			}
		}
	}
	// Gli ID devono essere distinti tra nomi distinti (nessuna collisione/duplicato).
	seen := map[uint32]string{}
	for _, name := range names {
		id := ref[name]
		if other, dup := seen[id]; dup {
			t.Errorf("ID duplicato %d per %q e %q", id, name, other)
		}
		seen[id] = name
	}
}
