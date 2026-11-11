package intreccio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/wire"
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

// remoteSubscriber is implemented by a backend that holds no data and so
// cannot evaluate a live query itself, but can have another member do it: a
// dataless cluster client. The stream it returns survives the loss of the
// member serving it (ADR 0011).
type remoteSubscriber interface {
	SubscribeRemote(ctx context.Context, req wire.Request) ([]string, wire.BatchStream, error)
}

// localSubscriberSetter lets a backend receive this DB's live-query server,
// used to run the live queries other members subscribe to on it.
type localSubscriberSetter interface {
	SetLocalSubscriber(fn func(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error)
}

// catchUpTimeout bounds how long a member waits to reach the index a
// subscriber asks it to start after, before telling it to try elsewhere.
var catchUpTimeout = 5 * time.Second

// catchUpPoll is how often that wait re-reads the applied index.
const catchUpPoll = 10 * time.Millisecond

// Subscribe starts a live query: it runs the read query once, delivers that
// result as the first batch, then keeps delivering the changes to the result as
// writes are committed, until the subscription is closed or ctx is done. The
// query may carry the STREAM prefix; it is implied here.
//
// Errors in the query — syntax, semantics, a missing parameter, a result over
// the row cap — are reported by Subscribe itself.
//
// On a dataless cluster client the query is evaluated by a voter. If that
// voter is lost the subscription moves to another one by itself, and the
// application sees only the net difference, if any.
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

	params, err := normalizeParams(params)
	if err != nil {
		return nil, err
	}
	q, err := parser.Parse(cypher)
	if err != nil {
		return nil, err
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return nil, err
	}
	if err := checkLiveQuery(q); err != nil {
		return nil, err
	}
	remote, isRemote := db.be.(remoteSubscriber)
	if !db.watchable && !isRemote {
		return nil, fmt.Errorf("subscribe: %w", ErrSubscribeUnsupported)
	}
	if db.isClosed() {
		return nil, fmt.Errorf("subscribe: %w", ErrClosed)
	}

	sctx, cancel := context.WithCancelCause(ctx)
	sub := &Subscription{
		changes: make(chan Change),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	// run is what the subscription's goroutine does: deliver batches until it
	// has to stop, and report why. cleanup releases what start acquired.
	var (
		run     func() error
		cleanup func()
	)
	if db.watchable {
		run, cleanup, err = db.startLocal(sctx, sub, q, semaRes.Columns, params, o.maxRows)
	} else {
		run, cleanup, err = db.startRemote(sctx, sub, remote, cypher, params, o.maxRows)
	}
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	if !db.track(sub) { // the database was closed meanwhile
		cleanup()
		cancel(nil)
		return nil, fmt.Errorf("subscribe: %w", ErrClosed)
	}

	go func() {
		err := run()
		// When the context ended, why it ended is the reason, whatever error
		// the interrupted work happened to return.
		switch cause := context.Cause(sctx); {
		case cause == nil: // run ended on its own: keep its error
		case errors.Is(cause, errSubscriptionClosed):
			err = nil
		case errors.Is(cause, ErrClosed):
			err = ErrClosed
		default:
			// The caller's context: report its plain error, so errors.Is works
			// whatever cancellation cause the caller attached.
			err = ctx.Err()
		}
		cleanup()
		db.untrack(sub)
		cancel(nil) // release the context if run ended on its own
		sub.err = err
		close(sub.changes)
		close(sub.done)
	}()
	return sub, nil
}

// checkLiveQuery reports whether q can be a live query: it must read the graph
// and nothing else. A writing query has no result to keep up to date, and a
// catalog listing (SHOW INDEXES / CONSTRAINTS) changes without any node or
// relationship changing, so nothing would ever wake it.
func checkLiveQuery(q *ast.Query) error {
	arms := [][]ast.Clause{q.Clauses}
	for _, u := range q.Unions {
		arms = append(arms, u.Clauses)
	}
	for _, arm := range arms {
		for _, c := range arm {
			if sema.IsWriteClause(c) {
				return errors.New("subscribe: a live query must be read-only")
			}
			switch c.(type) {
			case *ast.ShowIndexes, *ast.ShowConstraints:
				return errors.New("subscribe: a live query must read the graph: SHOW cannot be followed live")
			}
		}
	}
	return nil
}

// deliver sends c to the subscriber, or reports false if the subscription
// ended first.
func (s *Subscription) deliver(ctx context.Context, c Change) bool {
	select {
	case s.changes <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// startLocal starts a live query evaluated by this database.
func (db *DB) startLocal(ctx context.Context, sub *Subscription, q *ast.Query, columns []string, params map[string]any, maxRows int64) (run func() error, cleanup func(), err error) {
	lq := db.newLiveQuery(ctx, q, columns, params, maxRows)
	ev, err := lq.evaluate()
	if err != nil {
		lq.close()
		return nil, nil, err
	}
	sub.columns = columns

	run = func() error {
		if !sub.deliver(ctx, Change{Added: ev.added, Initial: true}) {
			return nil
		}
		for {
			ev, err := lq.next()
			if err != nil || ev == nil {
				return err
			}
			c := Change{Added: ev.added}
			for _, b := range ev.removed {
				row, err := decodeRow(b)
				if err != nil {
					return fmt.Errorf("subscription: %w", err)
				}
				c.Removed = append(c.Removed, row)
			}
			if !sub.deliver(ctx, c) {
				return nil
			}
		}
	}
	return run, lq.close, nil
}

// startRemote starts a live query evaluated by another member, for a backend
// that holds no data. The backend's stream hides the loss of that member.
func (db *DB) startRemote(ctx context.Context, sub *Subscription, remote remoteSubscriber, cypher string, params map[string]any, maxRows int64) (run func() error, cleanup func(), err error) {
	encParams, err := encodeParams(params)
	if err != nil {
		return nil, nil, err
	}
	columns, stream, err := remote.SubscribeRemote(ctx, wire.Request{Cypher: cypher, Params: encParams, MaxRows: maxRows})
	if err != nil {
		return nil, nil, fromRemote(err)
	}
	sub.columns = columns

	run = func() error {
		for {
			batch, err := stream.Recv()
			if err != nil {
				return fmt.Errorf("subscription: %w", fromRemote(err))
			}
			c := Change{Initial: batch.Initial}
			if c.Added, err = decodeRows(batch.Added); err != nil {
				return fmt.Errorf("subscription: %w", err)
			}
			if c.Removed, err = decodeRows(batch.Removed); err != nil {
				return fmt.Errorf("subscription: %w", err)
			}
			if !sub.deliver(ctx, c) {
				return nil
			}
		}
	}
	// The stream is bound to ctx: ending the subscription ends it.
	return run, func() {}, nil
}

// serveSubscription runs a live query on behalf of another member, sending its
// batches to sink until ctx ends or the query fails. It is wired into a
// clustered backend so a voter can serve the subscriptions of dataless clients.
//
// sink.Batch returns only when the subscriber is ready for more, so a slow
// subscriber is coalesced here exactly as a local one is.
func (db *DB) serveSubscription(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error {
	if !db.watchable || db.isClosed() {
		return &wire.Error{Code: wire.CodeUnavailable, Msg: "this member cannot serve live queries"}
	}
	params, err := decodeParams(req.Params)
	if err != nil {
		return err
	}
	q, err := parser.Parse(req.Cypher)
	if err != nil {
		return err
	}
	semaRes, err := sema.Analyze(q)
	if err != nil {
		return err
	}
	if err := checkLiveQuery(q); err != nil {
		return err
	}

	lq := db.newLiveQuery(ctx, q, semaRes.Columns, params, req.MaxRows)
	defer lq.close()
	if err := db.awaitApplied(ctx, req.AfterIndex); err != nil {
		return err
	}
	ev, err := lq.evaluate()
	if err != nil {
		return toRemote(err)
	}
	if err := sink.Columns(semaRes.Columns); err != nil {
		return err
	}
	if err := sink.Batch(wire.Batch{Initial: true, Index: ev.index, Added: ev.addedEnc}); err != nil {
		return err
	}
	for {
		ev, err := lq.next()
		if err != nil {
			return toRemote(err)
		}
		if ev == nil {
			return ctx.Err()
		}
		batch := wire.Batch{Index: ev.index, Added: ev.addedEnc, Removed: ev.removed}
		if err := sink.Batch(batch); err != nil {
			return err
		}
	}
}

// awaitApplied waits until this member has applied at least index, so that a
// subscriber arriving from another member does not see the result move
// backwards. It gives up after catchUpTimeout with a CodeBehind error.
func (db *DB) awaitApplied(ctx context.Context, index uint64) error {
	if index == 0 {
		return nil
	}
	deadline := time.Now().Add(catchUpTimeout)
	for {
		var applied uint64
		err := db.be.View(func(txn storage.Txn) error {
			var err error
			applied, err = catalog.AppliedIndex(txn)
			return err
		})
		if err != nil {
			return err
		}
		if applied >= index {
			return nil
		}
		if time.Now().After(deadline) {
			return &wire.Error{Code: wire.CodeBehind,
				Msg: fmt.Sprintf("this member has applied index %d, the subscriber needs %d", applied, index)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(catchUpPoll):
		}
	}
}

// liveQuery is a registered live query: the watcher that wakes it and the last
// result it produced. It is the core shared by local subscriptions and by the
// ones served to other members.
type liveQuery struct {
	db      *DB
	ctx     context.Context
	watcher *changefeed.Watcher
	q       *ast.Query
	columns []string
	params  map[string]any
	maxRows int64

	// Exactly one of the two holds the last result. A query that can be
	// re-evaluated around the nodes that changed has an anchor; any other is
	// re-run in full and diffed through result.
	anchor *anchor
	result changefeed.ResultSet
}

// newLiveQuery registers the watcher. That happens before the first
// evaluation on purpose: a commit landing in between then marks the watcher
// dirty instead of being lost.
func (db *DB) newLiveQuery(ctx context.Context, q *ast.Query, columns []string, params map[string]any, maxRows int64) *liveQuery {
	lq := &liveQuery{db: db, ctx: ctx, q: q, columns: columns, params: params, maxRows: maxRows}
	if anchoringEnabled {
		lq.anchor = newAnchor(q, columns)
	}
	if lq.anchor != nil {
		lq.watcher = db.hub.RegisterTracking(changefeed.FootprintOf(q))
	} else {
		lq.watcher = db.hub.Register(changefeed.FootprintOf(q))
	}
	return lq
}

func (lq *liveQuery) close() { lq.db.hub.Unregister(lq.watcher) }

// evaluation is how one evaluation of a live query changed its result. The
// first evaluation reports the whole result as added, in query order.
type evaluation struct {
	added    [][]any  // rows that entered the result
	addedEnc [][]byte // the same rows, canonically encoded
	removed  [][]byte // encoded rows that left it
	// index is the applied Raft index of the state the result was read from
	// (0 on an embedded database).
	index uint64
}

// evaluate brings the held result up to date with the current state.
func (lq *liveQuery) evaluate() (*evaluation, error) {
	if lq.anchor != nil {
		return lq.evaluateAnchored()
	}
	lq.db.liveStats.full.Add(1)
	ev := &evaluation{}
	var rows [][]any
	err := lq.db.be.View(func(txn storage.Txn) error {
		res, err := runQuery(txn, lq.q, lq.columns, lq.params, lq.ctx, lq.maxRows)
		if err != nil {
			return err
		}
		rows = res.Rows
		// Read in the same transaction as the rows: the index is exact.
		ev.index, err = catalog.AppliedIndex(txn)
		return err
	})
	if err != nil {
		return nil, err
	}
	enc, err := encodeRows(rows)
	if err != nil {
		return nil, err
	}
	var addedIdx []int
	addedIdx, ev.removed = lq.result.Replace(enc)
	for _, i := range addedIdx {
		ev.added = append(ev.added, rows[i])
		ev.addedEnc = append(ev.addedEnc, enc[i])
	}
	return ev, nil
}

// evaluateAnchored is evaluate for a query with an anchor: it re-evaluates only
// around the nodes that changed, and in full when those cannot be listed.
//
// Either way the held result ends up exactly the query's result on one
// snapshot. For the anchored path that takes care: a write is visible to a new
// read transaction from the moment it commits, a moment before the hub is told
// which nodes it touched. So the dirty set is taken twice — before the read
// transaction opens, and again once every write that transaction could contain
// has been reported (Hub.AwaitReported). Their union covers the snapshot. The
// second take may also hold writes that committed too late to be in it; those
// are put back, at the cost of one more round.
func (lq *liveQuery) evaluateAnchored() (*evaluation, error) {
	dirty, full := lq.watcher.TakeDirty()
	var (
		ev       *evaluation
		late     []uint64
		lateFull bool
	)
	err := lq.db.be.View(func(txn storage.Txn) error {
		var err error
		if lq.anchor.built && !full {
			if err = lq.db.hub.AwaitReported(lq.ctx, lq.db.hub.Started()); err != nil {
				return err
			}
			late, lateFull = lq.watcher.TakeDirty()
		}
		if !lq.anchor.built || full || lateFull {
			lq.db.liveStats.full.Add(1)
			ev, err = lq.anchor.rebuild(txn, lq)
		} else {
			lq.db.liveStats.anchored.Add(1)
			ev, err = lq.anchor.refresh(txn, lq, mergeIDs(dirty, late))
		}
		if err != nil {
			return err
		}
		ev.index, err = catalog.AppliedIndex(txn)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(late) > 0 || lateFull {
		// What the second take saw may not be in this snapshot: look again.
		lq.watcher.Remark(late, lateFull)
	}
	return ev, nil
}

// mergeIDs returns the sorted union of two sorted ID lists.
func mergeIDs(a, b []uint64) []uint64 {
	if len(b) == 0 {
		return a
	}
	out := make([]uint64, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		switch {
		case a[0] < b[0]:
			out, a = append(out, a[0]), a[1:]
		case a[0] > b[0]:
			out, b = append(out, b[0]), b[1:]
		default:
			out, a, b = append(out, a[0]), a[1:], b[1:]
		}
	}
	return append(append(out, a...), b...)
}

// next waits for the next evaluation that changed the result. It returns nil
// when the context ended. A wake-up that arrives while the caller is busy
// delivering stays pending, so the next evaluation covers every commit since.
func (lq *liveQuery) next() (*evaluation, error) {
	for {
		select {
		case <-lq.ctx.Done():
			return nil, nil
		case <-lq.watcher.Dirty():
		}
		ev, err := lq.evaluate()
		if err != nil {
			if lq.ctx.Err() != nil {
				return nil, nil
			}
			return nil, fmt.Errorf("subscription: %w", err)
		}
		if len(ev.added) > 0 || len(ev.removed) > 0 {
			return ev, nil
		}
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
