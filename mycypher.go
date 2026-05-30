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

// Backend is the storage/execution backend behind a DB. It is implemented by
// the default local engine and by the optional clustering package; end users do
// not implement it directly (use Open or cluster.Open). It exists so the
// clustering code can route writes through Raft without the core package
// depending on it.
type Backend interface {
	// View runs fn in a read-only transaction.
	View(fn func(storage.Txn) error) error
	// ApplyWrite executes a write via stage and durably commits/replicates its
	// effects, returning whatever stage produced.
	ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error)
	// Close releases the backend.
	Close() error
}

// DB is the database handle.
type DB struct {
	be Backend
}

// New wraps a Backend in a DB. It is the seam used by the clustering package;
// most callers use Open/OpenInMemory instead.
func New(be Backend) *DB { return &DB{be: be} }

// Open opens (or creates) the database in the given directory.
func Open(path string) (*DB, error) {
	store, err := badgerstore.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{be: &localBackend{store: store}}, nil
}

// OpenInMemory opens a fully in-RAM database (useful in tests).
func OpenInMemory() (*DB, error) {
	store, err := badgerstore.OpenInMemory()
	if err != nil {
		return nil, err
	}
	return &DB{be: &localBackend{store: store}}, nil
}

// Close releases the database resources.
func (db *DB) Close() error {
	return db.be.Close()
}

// Result is the outcome of a read query.
type Result struct {
	Columns []string
	Rows    [][]any
}

// Query runs a Cypher query and returns the result. If the query contains any
// write clauses (CREATE/MERGE/SET/DELETE) it runs as a write (committed locally,
// or replicated when clustered); otherwise it runs as a read.
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

	stage := func(txn storage.Txn) (any, error) {
		return runQuery(txn, q, semaRes.Columns, params)
	}

	if isWriteQuery(q) {
		res, err := db.be.ApplyWrite(stage)
		if err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
		return res.(*Result), nil
	}

	var result *Result
	err = db.be.View(func(txn storage.Txn) error {
		r, err := runQuery(txn, q, semaRes.Columns, params)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return result, nil
}

// runQuery plans and executes an already-parsed, already-analyzed query against
// txn. It is the single execution path shared by the local and clustered
// backends.
func runQuery(txn storage.Txn, q *ast.Query, columns []string, params map[string]any) (*Result, error) {
	p, err := plan.Plan(q, planCatalog{txn: txn})
	if err != nil {
		return nil, err
	}
	rows, err := exec.Run(p, columns, &exec.Context{Txn: txn, Params: params})
	if err != nil {
		return nil, err
	}
	return &Result{Columns: columns, Rows: rows}, nil
}

// Explain returns the textual physical plan for the given query without running
// it. Useful to verify that a plan picks up an index, an ordering, etc.
func (db *DB) Explain(ctx context.Context, cypher string) (string, error) {
	_ = ctx
	q, err := parser.Parse(cypher)
	if err != nil {
		return "", err
	}
	if _, err := sema.Analyze(q); err != nil {
		return "", err
	}
	var out string
	err = db.be.View(func(txn storage.Txn) error {
		p, err := plan.Plan(q, planCatalog{txn: txn})
		if err != nil {
			return err
		}
		out = plan.Explain(p)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("explain: %w", err)
	}
	return out, nil
}

func isWriteQuery(q *ast.Query) bool {
	for _, c := range q.Clauses {
		switch c.(type) {
		case *ast.Create, *ast.Merge, *ast.Set, *ast.Remove, *ast.Delete, *ast.CreateIndex:
			return true
		}
	}
	return false
}

// localBackend is the default, non-clustered backend: a local store where writes
// commit directly.
type localBackend struct {
	store storage.Store
}

var _ Backend = (*localBackend)(nil)

func (b *localBackend) View(fn func(storage.Txn) error) error { return b.store.View(fn) }

func (b *localBackend) ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error) {
	var result any
	err := b.store.Update(func(txn storage.Txn) error {
		r, err := stage(txn)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (b *localBackend) Close() error { return b.store.Close() }

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
