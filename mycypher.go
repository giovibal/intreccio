// Package mycypher is the public embeddable API of the graph DB.
package mycypher

import (
	"context"
	"fmt"

	"github.com/giovibal/mycypher/internal/catalog"
	"github.com/giovibal/mycypher/internal/cypher/ast"
	"github.com/giovibal/mycypher/internal/cypher/exec"
	"github.com/giovibal/mycypher/internal/cypher/parser"
	"github.com/giovibal/mycypher/internal/cypher/plan"
	"github.com/giovibal/mycypher/internal/cypher/sema"
	"github.com/giovibal/mycypher/internal/storage"
	badgerstore "github.com/giovibal/mycypher/internal/storage/badger"
)

// DB is the database handle.
type DB struct {
	store storage.Store
}

// Open opens (or creates) the database in the given directory.
func Open(path string) (*DB, error) {
	store, err := badgerstore.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{store: store}, nil
}

// OpenInMemory opens a fully in-RAM database (useful in tests).
func OpenInMemory() (*DB, error) {
	store, err := badgerstore.OpenInMemory()
	if err != nil {
		return nil, err
	}
	return &DB{store: store}, nil
}

// Close releases the database resources.
func (db *DB) Close() error {
	return db.store.Close()
}

// Result is the outcome of a read query.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Query runs a Cypher query and returns the result. If the query contains any
// write clauses (CREATE/MERGE/SET/DELETE) it runs inside a write transaction;
// otherwise inside a read-only transaction.
func (db *DB) Query(ctx context.Context, cypher string, params map[string]any) (*Result, error) {
	_ = ctx // reserved for cancellation; not consulted yet.
	q, err := parser.Parse(cypher)
	if err != nil {
		return nil, err
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, err
	}

	run := db.store.View
	if isWriteQuery(q) {
		run = db.store.Update
	}

	var result *Result
	err = run(func(txn storage.Txn) error {
		p, err := plan.Plan(q, planCatalog{txn: txn})
		if err != nil {
			return err
		}
		rows, err := exec.Run(p, semaRes.Columns, &exec.Context{Txn: txn, Params: params})
		if err != nil {
			return err
		}
		result = &Result{Columns: semaRes.Columns, Rows: rows}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return result, nil
}

func isWriteQuery(q *ast.Query) bool {
	for _, c := range q.Clauses {
		switch c.(type) {
		case *ast.Create, *ast.Merge, *ast.Set, *ast.Delete, *ast.CreateIndex:
			return true
		}
	}
	return false
}

// planCatalog adapts the catalog to plan.Catalog, resolving names within the
// transaction. A missing label/key means no index, which is correct.
type planCatalog struct {
	txn storage.Txn
}

func (c planCatalog) HasIndex(label, propKey string) bool {
	labelID, found, err := catalog.LookupLabel(c.txn, label)
	if err != nil || !found {
		return false
	}
	keyID, found, err := catalog.LookupKey(c.txn, propKey)
	if err != nil || !found {
		return false
	}
	has, err := catalog.HasIndex(c.txn, labelID, keyID)
	return err == nil && has
}
