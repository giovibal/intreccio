package intreccio

import (
	"context"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/sema"
)

// ErrReadOnlyTx is returned by Tx.Query for a write query inside View.
var ErrReadOnlyTx = errors.New("intreccio: write query in a read-only transaction")

// ErrTxDone is returned by Tx.Query once the transaction function returned.
var ErrTxDone = errors.New("intreccio: transaction already finished")

// ErrTxAborted is returned once a write query inside DB.Update has failed
// while executing, since it may have made part of its changes: by every later
// Tx.Query in the same transaction, and by Update when its function returns
// nil anyway, in which case Update commits nothing. (When the function
// returns an error of its own, Update returns that error and also commits
// nothing.) The error wraps the failure of that write query.
var ErrTxAborted = errors.New("intreccio: transaction aborted by a failed write query")

// ErrTxNotOnLeader is returned by Update on a cluster node that is not the
// leader (and by Update and View on a dataless client): a transaction runs Go
// code between statements, so unlike Query it cannot be forwarded.
var ErrTxNotOnLeader = errors.New("intreccio: transactions must run on the cluster leader")

// Tx is a transaction spanning several queries, passed to the function given
// to DB.Update or DB.View. Every query sees the writes of the earlier queries
// of the same transaction. A Tx must not be used concurrently or after the
// function returns.
type Tx struct {
	txn      storage.Txn
	readOnly bool
	done     bool
	// failed is the error of the first write query that failed. A write
	// query stops wherever its error occurs, possibly after some of its
	// changes, so from then on the transaction can only be rolled back.
	failed error
}

// aborted is the error of a transaction a write query has failed in.
func (tx *Tx) aborted() error {
	return fmt.Errorf("%w: %w", ErrTxAborted, tx.failed)
}

// Query runs one Cypher query inside the transaction. WithMaxRows applies;
// Linearizable has no effect (the transaction already fixes its snapshot).
//
// A write query that fails while executing aborts the transaction: it may
// have made part of its changes, so they cannot be kept. Later queries fail
// with ErrTxAborted and Update commits nothing. A query refused before it
// runs (a syntax, semantic or planning error, a write in View, ...) and a
// failed read change nothing and leave the transaction usable.
func (tx *Tx) Query(ctx context.Context, cypher string, params map[string]any, opts ...QueryOption) (*Result, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	if tx.failed != nil {
		return nil, tx.aborted()
	}
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
	params, err := normalizeParams(params)
	if err != nil {
		return nil, err
	}
	q, err := parser.Parse(cypher)
	if err != nil {
		return nil, err
	}
	if q.Stream {
		// A live query has no place in a transaction: it outlives it.
		return nil, fmt.Errorf("query: %w", ErrStreamQuery)
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, err
	}
	if tx.readOnly && isWriteQuery(q) {
		return nil, ErrReadOnlyTx
	}
	pq, err := prepareQuery(tx.txn, q, semaRes.Columns, params)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err) // nothing ran
	}
	res, err := pq.run(tx.txn, ctx, o.maxRows)
	if err != nil {
		err = fmt.Errorf("query: %w", err)
		if isWriteQuery(q) {
			tx.failed = err
		}
		return nil, err
	}
	return res, nil
}

// Update runs fn in one read-write transaction: every query in it commits
// together when fn returns nil, and nothing is written when it returns an
// error (which Update returns). Nothing is written either when a write query
// in it failed while executing, even if fn handled that error and returned
// nil: Update then returns an error wrapping ErrTxAborted. In cluster mode all its writes are
// replicated as one entry; it must run on the leader (ErrTxNotOnLeader).
//
// fn may be called more than once: when the transaction conflicts with a
// concurrent write, it is retried from the start on fresh data. It must not
// have effects outside the transaction that cannot be repeated.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cb, ok := db.be.(clusterBackend); ok && (!cb.Local() || !cb.IsLeader()) {
		return ErrTxNotOnLeader
	}
	_, err := db.be.ApplyWrite(func(txn storage.Txn) (any, error) {
		tx := &Tx{txn: txn}
		defer func() { tx.done = true }()
		if err := fn(tx); err != nil {
			return nil, err
		}
		if tx.failed != nil {
			return nil, tx.aborted() // roll back what the failed query left
		}
		return nil, nil
	})
	return fromRemote(err) // ErrWriteOutcomeUnknown, if replication was cut short
}

// View runs fn in one read-only transaction: all its queries see the same
// consistent snapshot. Write queries fail with ErrReadOnlyTx. In cluster mode
// the snapshot is the local replica's (possibly slightly stale, as with a
// default Query).
func (db *DB) View(ctx context.Context, fn func(tx *Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cb, ok := db.be.(clusterBackend); ok && !cb.Local() {
		return ErrTxNotOnLeader
	}
	return db.be.View(func(txn storage.Txn) error {
		tx := &Tx{txn: txn, readOnly: true}
		defer func() { tx.done = true }()
		return fn(tx)
	})
}
