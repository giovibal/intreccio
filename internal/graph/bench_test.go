package graph

import (
	"fmt"
	"testing"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/storage"
	badgeradapter "github.com/giovibal/mycypher/internal/storage/badger"
)

func newBenchStore(b *testing.B) storage.Store {
	b.Helper()
	s, err := badgeradapter.OpenInMemory()
	if err != nil {
		b.Fatalf("OpenInMemory: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// BenchmarkCreateNode measures the cost of creating a single node (one Update
// transaction per node).
func BenchmarkCreateNode(b *testing.B) {
	s := newBenchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Update(func(tx storage.Txn) error {
			_, err := CreateNode(tx, []string{"Person"}, map[string]any{"name": fmt.Sprintf("p%d", i)})
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOutEdges measures the cost of enumerating the outgoing edges of a
// node with a 100-edge fan-out.
func BenchmarkOutEdges(b *testing.B) {
	s := newBenchStore(b)
	var root uint64
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		root, err = CreateNode(tx, []string{"Hub"}, nil)
		if err != nil {
			return err
		}
		for i := 0; i < 100; i++ {
			child, err := CreateNode(tx, []string{"Leaf"}, nil)
			if err != nil {
				return err
			}
			if _, err := CreateEdge(tx, "C", root, child, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.View(func(tx storage.Txn) error {
			_, err := OutEdges(tx, root, 0)
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNodesByPropertyIndexed measures indexed point lookup over 1000 nodes.
func BenchmarkNodesByPropertyIndexed(b *testing.B) {
	const n = 1000
	s := newBenchStore(b)
	var labelID, keyID uint32
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		if labelID, err = catalog.InternLabel(tx, "Person"); err != nil {
			return err
		}
		if keyID, err = catalog.InternKey(tx, "email"); err != nil {
			return err
		}
		if err := catalog.AddIndex(tx, labelID, keyID); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if _, err := CreateNode(tx, []string{"Person"}, map[string]any{"email": fmt.Sprintf("p%d@x.com", i)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.View(func(tx storage.Txn) error {
			_, err := NodesByProperty(tx, labelID, keyID, fmt.Sprintf("p%d@x.com", i%n))
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNodesByPropertyFallback measures the no-index path over 1000 nodes
// (label scan + filter) for comparison with the indexed version.
func BenchmarkNodesByPropertyFallback(b *testing.B) {
	const n = 1000
	s := newBenchStore(b)
	var labelID, keyID uint32
	if err := s.Update(func(tx storage.Txn) error {
		var err error
		if labelID, err = catalog.InternLabel(tx, "Person"); err != nil {
			return err
		}
		if keyID, err = catalog.InternKey(tx, "email"); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if _, err := CreateNode(tx, []string{"Person"}, map[string]any{"email": fmt.Sprintf("p%d@x.com", i)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.View(func(tx storage.Txn) error {
			_, err := NodesByProperty(tx, labelID, keyID, fmt.Sprintf("p%d@x.com", i%n))
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}
