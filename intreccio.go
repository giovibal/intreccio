// Package intreccio is the public embeddable API of the graph DB.
package intreccio

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/exec"
	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
	"github.com/giovibal/intreccio/internal/wire"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/plan"
	"github.com/giovibal/intreccio/query/sema"
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
	// hub is the registry of live-query watchers. Commit points report to it;
	// it costs nothing while no watcher is registered.
	hub *changefeed.Hub
	// watchable is set when the backend reports its commits to hub, i.e. when
	// Subscribe can work.
	watchable bool

	mu     sync.Mutex
	subs   map[*Subscription]struct{} // running subscriptions
	closed bool
}

// New wraps a Backend in a DB. It is the seam used by the clustering package;
// most callers use Open/OpenInMemory instead. If the backend supports forwarding
// of leader-bound queries, New wires this DB's local executor into it.
func New(be Backend) *DB {
	db := &DB{be: be}
	db.hub = changefeed.NewHub(dictionary{view: be.View})
	if r, ok := be.(changeReporter); ok {
		r.SetChangeHub(db.hub)
		db.watchable = true
	}
	if s, ok := be.(localExecutorSetter); ok {
		s.SetLocalExecutor(db.executeLocal)
	}
	return db
}

// dictionary resolves dictionary IDs to names through short read transactions.
// It is the hub's changefeed.Resolver; the hub caches what it returns.
type dictionary struct {
	view func(fn func(storage.Txn) error) error
}

func (d dictionary) name(resolve func(storage.Txn, uint32) (string, error), id uint32) (string, error) {
	var name string
	err := d.view(func(txn storage.Txn) error {
		var err error
		name, err = resolve(txn, id)
		return err
	})
	return name, err
}

func (d dictionary) LabelName(id uint32) (string, error) { return d.name(catalog.LabelName, id) }
func (d dictionary) TypeName(id uint32) (string, error)  { return d.name(catalog.TypeName, id) }
func (d dictionary) KeyName(id uint32) (string, error)   { return d.name(catalog.KeyName, id) }

// clusterBackend is the optional capability set of a clustered backend. The core
// package never imports the clustering package; it discovers these methods via
// an interface assertion so the embedded path is unaffected.
type clusterBackend interface {
	IsLeader() bool
	// Local reports whether the backend can serve reads from a local store. A
	// dataless client returns false, so every query is forwarded.
	Local() bool
	ReadBarrier() error
	// Forward runs the request on another member and returns its rows in the
	// canonical row encoding (see rowcodec.go). The backend carries parameters
	// and rows as opaque bytes. Ending ctx stops the remote execution too.
	Forward(ctx context.Context, req wire.Request) ([]string, [][]byte, error)
}

// localExecutorSetter lets a backend receive this DB's local executor (used to
// run the requests other members forward to it). The executor takes parameters
// and returns rows in the canonical row encoding, so the backend never needs to
// know the value model.
type localExecutorSetter interface {
	SetLocalExecutor(fn func(ctx context.Context, req wire.Request) ([]string, [][]byte, error))
}

// QueryOption configures a single Query call.
type QueryOption func(*queryOptions)

type queryOptions struct {
	linearizable bool
	maxRows      int64
}

// Linearizable requests a strongly-consistent read (read-your-writes across the
// cluster), served via the leader. It has no effect on writes or on a
// non-clustered database (local reads are already consistent there).
func Linearizable() QueryOption { return func(o *queryOptions) { o.linearizable = true } }

// WithMaxRows caps the number of rows a read query may produce. If the query
// yields more, it aborts with ErrResultLimit. It is a safety net against
// unbounded result sets (mind the memory an ORDER BY or aggregation still needs
// before rows are emitted). A limit <= 0 means no cap, which is the default.
func WithMaxRows(limit int) QueryOption {
	return func(o *queryOptions) {
		if limit > 0 {
			o.maxRows = int64(limit)
		}
	}
}

// ErrResultLimit is returned by Query when a result exceeds WithMaxRows.
var ErrResultLimit = exec.ErrResultLimit

// OpenOption configures how Open opens a database.
type OpenOption func(*openOptions)

type openOptions struct{ syncWrites bool }

// WithSyncWrites controls whether committed writes are made durable (fsynced)
// before Query returns. It is enabled by default, so writes survive an OS crash
// or power loss. Passing false trades that durability for higher write
// throughput: recent writes may then be lost after a crash, though the store
// itself stays consistent. It has no effect on an in-memory database.
func WithSyncWrites(on bool) OpenOption {
	return func(o *openOptions) { o.syncWrites = on }
}

// Open opens (or creates) the database in the given directory. Writes are made
// durable by default; pass WithSyncWrites(false) to trade durability for speed.
func Open(path string, opts ...OpenOption) (*DB, error) {
	o := openOptions{syncWrites: true}
	for _, opt := range opts {
		opt(&o)
	}
	store, err := badgerstore.OpenWithOptions(path, badgerstore.Options{SyncWrites: o.syncWrites})
	if err != nil {
		return nil, err
	}
	if err := ensureFormat(store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return New(&localBackend{store: store}), nil
}

// OpenInMemory opens a fully in-RAM database (useful in tests).
func OpenInMemory() (*DB, error) {
	store, err := badgerstore.OpenInMemory()
	if err != nil {
		return nil, err
	}
	if err := ensureFormat(store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return New(&localBackend{store: store}), nil
}

// ensureFormat initialises the on-disk format marker on a fresh database and
// verifies it on an existing one, so an incompatible layout is rejected at open.
func ensureFormat(store storage.Store) error {
	return store.Update(func(txn storage.Txn) error {
		return catalog.EnsureFormat(txn)
	})
}

// Close releases the database resources. Running subscriptions are ended first
// (their Err is ErrClosed).
func (db *DB) Close() error {
	db.closeSubscriptions()
	return db.be.Close()
}

// Result is the outcome of a query. Rows holds one slice per row, in column
// order. Each value is one of: nil, bool, int64, float64, string, []any,
// map[string]any, Node or Relationship (lists and maps may nest any of these).
type Result struct {
	Columns []string
	Rows    [][]any
}

// Query runs a Cypher query and returns the result. If the query contains any
// write clauses (CREATE/MERGE/SET/DELETE) it runs as a write (committed locally,
// or replicated through the leader when clustered); otherwise it runs as a read.
// Pass Linearizable() for a strongly-consistent clustered read.
func (db *DB) Query(ctx context.Context, cypher string, params map[string]any, opts ...QueryOption) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	var o queryOptions
	for _, opt := range opts {
		opt(&o)
	}

	q, err := parser.Parse(cypher)
	if err != nil {
		return nil, err
	}
	if q.Stream {
		return nil, fmt.Errorf("query: %w", ErrStreamQuery)
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, err
	}
	write := isWriteQuery(q)

	// Clustered routing. A dataless client forwards everything. A voter serves
	// reads locally and writes/linearizable reads via the leader (forwarding when
	// it is not the leader; issuing a read barrier first when it is).
	if cb, ok := db.be.(clusterBackend); ok {
		forward := !cb.Local() ||
			(write && !cb.IsLeader()) ||
			(o.linearizable && !write && !cb.IsLeader())
		if forward {
			encParams, err := encodeParams(params)
			if err != nil {
				return nil, fmt.Errorf("query: %w", err)
			}
			req := wire.Request{
				Cypher:       cypher,
				Params:       encParams,
				Write:        write,
				Linearizable: o.linearizable && !write,
			}
			if !write {
				req.MaxRows = o.maxRows // writes are never capped, as locally
			}
			cols, enc, err := cb.Forward(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("query: %w", fromRemote(err))
			}
			rows, err := decodeRows(enc)
			if err != nil {
				return nil, fmt.Errorf("query: %w", err)
			}
			return &Result{Columns: cols, Rows: rows}, nil
		}
		if o.linearizable && !write && cb.IsLeader() {
			if err := cb.ReadBarrier(); err != nil {
				return nil, fmt.Errorf("query: %w", err)
			}
		}
	}

	res, err := db.runLocal(ctx, q, semaRes.Columns, params, write, o.maxRows)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return res, nil
}

// runLocal executes an already-parsed query against the local backend: writes go
// through ApplyWrite (replicated when clustered, committed when embedded), reads
// through View. ctx bounds execution and maxRows caps the result.
func (db *DB) runLocal(ctx context.Context, q *ast.Query, columns []string, params map[string]any, write bool, maxRows int64) (*Result, error) {
	if write {
		res, err := db.be.ApplyWrite(func(txn storage.Txn) (any, error) {
			return runQuery(txn, q, columns, params, ctx, 0)
		})
		if err != nil {
			return nil, err
		}
		return res.(*Result), nil
	}
	var result *Result
	err := db.be.View(func(txn storage.Txn) error {
		r, err := runQuery(txn, q, columns, params, ctx, maxRows)
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

// executeLocal parses and runs a forwarded request entirely on the local node,
// returning the columns and the rows in the canonical row encoding. It is wired
// into a clustered backend so a member can execute the queries others forward
// to it; it never forwards. ctx bounds the execution.
func (db *DB) executeLocal(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	params, err := decodeParams(req.Params)
	if err != nil {
		return nil, nil, err
	}
	q, err := parser.Parse(req.Cypher)
	if err != nil {
		return nil, nil, err
	}
	if q.Stream {
		return nil, nil, ErrStreamQuery
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, nil, err
	}
	write := isWriteQuery(q)
	maxRows := req.MaxRows
	if write {
		maxRows = 0
	}
	res, err := db.runLocal(ctx, q, semaRes.Columns, params, write, maxRows)
	if err != nil {
		return nil, nil, toRemote(err)
	}
	enc, err := encodeRows(res.Rows)
	if err != nil {
		return nil, nil, err
	}
	return res.Columns, enc, nil
}

// toRemote tags an error that the requesting member must be able to recognise
// with its wire code; any other error travels as its message.
func toRemote(err error) error {
	if errors.Is(err, ErrResultLimit) {
		return &wire.Error{Code: wire.CodeResultLimit, Msg: err.Error()}
	}
	return err
}

// fromRemote is the inverse of toRemote on the requesting member.
func fromRemote(err error) error {
	var remote *wire.Error
	if errors.As(err, &remote) && remote.Code == wire.CodeResultLimit {
		return ErrResultLimit
	}
	return err
}

// encodeParams serializes a query's parameter map for forwarding, as a
// one-value row. A nil map stays empty bytes.
func encodeParams(params map[string]any) ([]byte, error) {
	if len(params) == 0 {
		return nil, nil
	}
	return encodeRow([]any{params})
}

// decodeParams is the inverse of encodeParams.
func decodeParams(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	row, err := decodeRow(b)
	if err != nil {
		return nil, err
	}
	if len(row) != 1 {
		return nil, errors.New("intreccio: malformed parameters")
	}
	params, ok := row[0].(map[string]any)
	if !ok {
		return nil, errors.New("intreccio: malformed parameters")
	}
	return params, nil
}

// runQuery plans and executes an already-parsed, already-analyzed query against
// txn. It is the single execution path shared by the local and clustered
// backends. The rows are exported to the public value model (Node,
// Relationship) while txn is still open, since that needs the dictionaries.
func runQuery(txn storage.Txn, q *ast.Query, columns []string, params map[string]any, ctx context.Context, maxRows int64) (*Result, error) {
	p, err := plan.Plan(q, planCatalog{txn: txn})
	if err != nil {
		return nil, err
	}
	rows, err := exec.Run(p, columns, &exec.Context{Txn: txn, Params: params, Ctx: ctx, MaxRows: maxRows})
	if err != nil {
		return nil, err
	}
	if err := newExporter(txn).rows(rows); err != nil {
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
		if sema.IsWriteClause(c) {
			return true
		}
	}
	return false
}

// localBackend is the default, non-clustered backend: a local store where writes
// commit directly. It is the embedded commit point for live queries: every
// committed write is reported to the hub.
type localBackend struct {
	store storage.Store
	hub   *changefeed.Hub
}

var (
	_ Backend        = (*localBackend)(nil)
	_ changeReporter = (*localBackend)(nil)
)

// SetChangeHub wires the hub this backend reports its commits to.
func (b *localBackend) SetChangeHub(h *changefeed.Hub) { b.hub = h }

func (b *localBackend) View(fn func(storage.Txn) error) error { return b.store.View(fn) }

func (b *localBackend) ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error) {
	var result any
	// hub.Update commits through the store and then reports the write.
	err := b.hub.Update(b.store, func(txn storage.Txn) error {
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
