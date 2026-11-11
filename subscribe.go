package intreccio

import (
	"context"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/query/ast"
	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/sema"
)

// DefaultSubscriptionMaxRows is the default cap on the number of rows a live
// query's result may hold. See WithSubscriptionMaxRows.
const DefaultSubscriptionMaxRows = 10000

var (
	// ErrStreamQuery is returned by Query for a query carrying the STREAM
	// prefix: a live query has no single result and must be run with Subscribe.
	ErrStreamQuery = errors.New("a STREAM query is a live query: run it with Subscribe")

	// ErrSubscribeUnsupported is returned by Subscribe when the database's
	// backend cannot serve live queries.
	ErrSubscribeUnsupported = errors.New("live queries are not supported by this backend")

	// ErrClosed is returned by Subscribe on a closed database, and is the error
	// of a subscription that ended because its database was closed.
	ErrClosed = errors.New("database is closed")

	// errSubscriptionClosed is the internal cancellation cause of Close; it is
	// reported to the caller as a nil error.
	errSubscriptionClosed = errors.New("subscription closed")
)

// Change is one batch of changes to the result of a live query. Rows have the
// same shape as Result.Rows, in the column order of Subscription.Columns.
//
// The result is a multiset of rows: Added lists rows that entered it and
// Removed rows that left it. A row whose contents changed appears as its old
// version in Removed and its new version in Added, in the same batch. Rows that
// only changed position are not reported.
type Change struct {
	Added   [][]any
	Removed [][]any
	// Initial is set on the first batch only: the result as it was when the
	// subscription started, all in Added, in the order the query produced it.
	Initial bool
}

// Subscription is a live query: a read query whose result is kept up to date.
// Its batches are read from Changes. Close it, or cancel the context given to
// Subscribe, to release it.
type Subscription struct {
	columns []string
	changes chan Change
	cancel  context.CancelCauseFunc
	done    chan struct{} // closed after err is set and changes is closed
	err     error
}

// Columns returns the names of the result columns.
func (s *Subscription) Columns() []string { return s.columns }

// Changes returns the channel on which batches are delivered. The first batch
// is the current result (Change.Initial); later ones describe how it changed.
// The channel is closed when the subscription ends; Err then tells why.
//
// Batches are coalesced. While the consumer is not receiving, any number of
// commits collapse into the net difference, delivered as soon as it receives
// again: a slow consumer skips intermediate states instead of queueing them.
func (s *Subscription) Changes() <-chan Change { return s.changes }

// Err reports why the subscription ended. It is nil while the subscription is
// live and after Close; otherwise it is the cause: the error of the context
// given to Subscribe (context.Canceled or context.DeadlineExceeded), an error
// wrapping ErrResultLimit when the result outgrew its row cap, ErrClosed when
// the database was closed, or the error of a failed evaluation.
func (s *Subscription) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close ends the subscription and waits until its resources are released. It
// is idempotent and always returns nil.
func (s *Subscription) Close() error {
	s.cancel(errSubscriptionClosed)
	<-s.done
	return nil
}

// SubscribeOption configures a single Subscribe call.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	maxRows int64
}

// WithSubscriptionMaxRows caps the number of rows the live query's result may
// hold; the default is DefaultSubscriptionMaxRows. A subscription keeps its
// last result in memory for as long as it lives, so the cap bounds that memory.
// If the result exceeds it, Subscribe fails — or, later, the subscription ends —
// with an error wrapping ErrResultLimit. A limit <= 0 removes the cap.
func WithSubscriptionMaxRows(limit int) SubscribeOption {
	return func(o *subscribeOptions) {
		if limit > 0 {
			o.maxRows = int64(limit)
		} else {
			o.maxRows = 0
		}
	}
}

// changeReporter is implemented by a backend whose commit points report every
// committed write to a changefeed.Hub, which is what makes live queries
// possible on it. The backend must report only after a successful commit and
// must report a commit it cannot describe as an unknown change.
type changeReporter interface {
	SetChangeHub(h *changefeed.Hub)
}

// Subscribe starts a live query: it runs the read query once, delivers that
// result as the first batch, then keeps delivering the changes to the result as
// writes are committed, until the subscription is closed or ctx is done. The
// query may carry the STREAM prefix; it is implied here.
//
// Errors in the query — syntax, semantics, a missing parameter, a result over
// the row cap — are reported by Subscribe itself.
func (db *DB) Subscribe(ctx context.Context, cypher string, params map[string]any, opts ...SubscribeOption) (*Subscription, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	o := subscribeOptions{maxRows: DefaultSubscriptionMaxRows}
	for _, opt := range opts {
		opt(&o)
	}

	q, err := parser.Parse(cypher)
	if err != nil {
		return nil, err
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, err
	}
	if isWriteQuery(q) {
		return nil, errors.New("subscribe: a live query must be read-only")
	}
	if !db.watchable {
		return nil, fmt.Errorf("subscribe: %w", ErrSubscribeUnsupported)
	}
	if db.isClosed() {
		return nil, fmt.Errorf("subscribe: %w", ErrClosed)
	}

	sctx, cancel := context.WithCancelCause(ctx)
	lq := &liveQuery{
		db:      db,
		parent:  ctx,
		ctx:     sctx,
		q:       q,
		columns: semaRes.Columns,
		params:  params,
		maxRows: o.maxRows,
	}
	// Register before the first evaluation: a commit landing in between then
	// marks the watcher dirty instead of being lost.
	lq.watcher = db.hub.Register(changefeed.FootprintOf(q))

	initial, _, err := lq.evaluate()
	if err != nil {
		db.hub.Unregister(lq.watcher)
		cancel(nil)
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	lq.sub = &Subscription{
		columns: semaRes.Columns,
		changes: make(chan Change),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	if !db.track(lq.sub) { // the database was closed meanwhile
		db.hub.Unregister(lq.watcher)
		cancel(nil)
		return nil, fmt.Errorf("subscribe: %w", ErrClosed)
	}
	go lq.run(initial)
	return lq.sub, nil
}

// liveQuery is the running state of one subscription: the watcher that wakes
// it and the last result it emitted.
type liveQuery struct {
	db      *DB
	parent  context.Context // the caller's context
	ctx     context.Context // derived from parent; ended by Close and by the database
	sub     *Subscription
	watcher *changefeed.Watcher
	q       *ast.Query
	columns []string
	params  map[string]any
	maxRows int64
	result  changefeed.ResultSet
}

// evaluate runs the query against the current state and folds the outcome into
// the held result. It returns the rows that are new and those that are gone.
func (lq *liveQuery) evaluate() (added, removed [][]any, err error) {
	var res *Result
	err = lq.db.be.View(func(txn storage.Txn) error {
		r, err := runQuery(txn, lq.q, lq.columns, lq.params, lq.ctx, lq.maxRows)
		if err != nil {
			return err
		}
		res = r
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	enc, err := encodeRows(res.Rows)
	if err != nil {
		return nil, nil, err
	}
	addedIdx, removedEnc := lq.result.Replace(enc)
	for _, i := range addedIdx {
		added = append(added, res.Rows[i])
	}
	for _, b := range removedEnc {
		row, err := decodeRow(b)
		if err != nil {
			return nil, nil, err
		}
		removed = append(removed, row)
	}
	return added, removed, nil
}

// run delivers the initial batch, then one batch per evaluation that changed
// the result. A wake-up that arrives while a batch is waiting to be received
// stays pending, so the next evaluation covers every commit since.
func (lq *liveQuery) run(initial [][]any) {
	err := lq.loop(initial)
	// When the context ended, why it ended is the reason, whatever error the
	// interrupted evaluation happened to return.
	switch cause := context.Cause(lq.ctx); {
	case cause == nil: // the loop ended on its own: keep its error
	case errors.Is(cause, errSubscriptionClosed):
		err = nil
	case errors.Is(cause, ErrClosed):
		err = ErrClosed
	default:
		// The caller's context: report its plain error, so errors.Is works
		// whatever cancellation cause the caller attached.
		err = lq.parent.Err()
	}
	lq.db.hub.Unregister(lq.watcher)
	lq.db.untrack(lq.sub)
	lq.sub.cancel(nil) // release the context if the loop ended on its own
	lq.sub.err = err
	close(lq.sub.changes)
	close(lq.sub.done)
}

func (lq *liveQuery) loop(initial [][]any) error {
	if !lq.send(Change{Added: initial, Initial: true}) {
		return nil
	}
	for {
		select {
		case <-lq.ctx.Done():
			return nil
		case <-lq.watcher.Dirty():
		}
		added, removed, err := lq.evaluate()
		if err != nil {
			return fmt.Errorf("subscription: %w", err)
		}
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		if !lq.send(Change{Added: added, Removed: removed}) {
			return nil
		}
	}
}

// send delivers c, or reports false if the subscription ended first.
func (lq *liveQuery) send(c Change) bool {
	select {
	case lq.sub.changes <- c:
		return true
	case <-lq.ctx.Done():
		return false
	}
}

// track records a running subscription so Close can end it. It reports false
// if the database is already closed.
func (db *DB) track(s *Subscription) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return false
	}
	if db.subs == nil {
		db.subs = map[*Subscription]struct{}{}
	}
	db.subs[s] = struct{}{}
	return true
}

func (db *DB) untrack(s *Subscription) {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.subs, s)
}

func (db *DB) isClosed() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.closed
}

// closeSubscriptions marks the database closed and ends every running
// subscription with ErrClosed, waiting for each to release its resources.
func (db *DB) closeSubscriptions() {
	db.mu.Lock()
	db.closed = true
	subs := make([]*Subscription, 0, len(db.subs))
	for s := range db.subs {
		subs = append(subs, s)
	}
	db.mu.Unlock()
	for _, s := range subs {
		s.cancel(ErrClosed)
		<-s.done
	}
}
