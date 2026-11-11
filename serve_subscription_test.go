package intreccio

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/wire"
)

// fakeSink records what a served live query sends. Batch blocks until the test
// releases it, standing in for a subscriber's acknowledgement.
type fakeSink struct {
	cols    chan []string
	batches chan wire.Batch
	acks    chan struct{}
	ctx     context.Context
}

func newFakeSink(ctx context.Context) *fakeSink {
	return &fakeSink{cols: make(chan []string, 1), batches: make(chan wire.Batch, 16), acks: make(chan struct{}, 16), ctx: ctx}
}

func (s *fakeSink) Columns(cols []string) error { s.cols <- cols; return nil }

func (s *fakeSink) Batch(b wire.Batch) error {
	s.batches <- b
	select {
	case <-s.acks:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *fakeSink) next(t *testing.T) wire.Batch {
	t.Helper()
	select {
	case b := <-s.batches:
		return b
	case <-time.After(subTimeout):
		t.Fatal("timed out waiting for a served batch")
	}
	return wire.Batch{}
}

func decodeAll(t *testing.T, enc [][]byte) [][]any {
	t.Helper()
	rows, err := decodeRows(enc)
	if err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	return rows
}

// setApplied stands in for a voter's FSM, which records the Raft index of each
// write next to the data.
func setApplied(t *testing.T, db *DB, index uint64) {
	t.Helper()
	if _, err := db.be.ApplyWrite(func(txn storage.Txn) (any, error) {
		return nil, catalog.SetAppliedIndex(txn, index)
	}); err != nil {
		t.Fatalf("set applied index: %v", err)
	}
}

// serve runs serveSubscription in the background and returns its sink and a
// channel with its final error.
func serve(t *testing.T, db *DB, req wire.SubscribeRequest) (*fakeSink, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sink := newFakeSink(ctx)
	done := make(chan error, 1)
	go func() { done <- db.serveSubscription(ctx, req, sink) }()
	return sink, cancel, done
}

func TestServeSubscription(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'Ann'})")
	setApplied(t, db, 7)

	sink, cancel, done := serve(t, db, wire.SubscribeRequest{Request: wire.Request{
		Cypher: "STREAM MATCH (p:Person) RETURN p.name AS name"}})
	if cols := <-sink.cols; len(cols) != 1 || cols[0] != "name" {
		t.Fatalf("columns = %v", cols)
	}
	first := sink.next(t)
	if !first.Initial || first.Index != 7 || len(first.Removed) != 0 {
		t.Fatalf("initial batch = %+v", first)
	}
	if got := decodeAll(t, first.Added); len(got) != 1 || got[0][0] != "Ann" {
		t.Fatalf("initial rows = %v", got)
	}

	// While the subscriber has not acknowledged, commits coalesce.
	mustExec(t, db, "CREATE (:Person {name: 'Bob'})")
	mustExec(t, db, "CREATE (:Person {name: 'Cy'})")
	mustExec(t, db, "MATCH (p:Person {name: 'Bob'}) DELETE p")
	setApplied(t, db, 11)
	sink.acks <- struct{}{}
	b := sink.next(t)
	if b.Initial || b.Index != 11 {
		t.Fatalf("second batch = %+v", b)
	}
	added, removed := decodeAll(t, b.Added), decodeAll(t, b.Removed)
	// Either the net difference in one batch, or Bob in and out across two.
	if len(added) == 2 {
		sink.acks <- struct{}{}
		b = sink.next(t)
		removed = decodeAll(t, b.Removed)
		if len(removed) != 1 || removed[0][0] != "Bob" {
			t.Fatalf("follow-up batch = %+v", b)
		}
	} else if len(added) != 1 || added[0][0] != "Cy" || len(removed) != 0 {
		t.Fatalf("coalesced batch: added %v removed %v", added, removed)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("serveSubscription returned %v after cancellation", err)
		}
	case <-time.After(subTimeout):
		t.Fatal("serveSubscription did not stop")
	}
	if db.hub.Active() {
		t.Error("the served live query left its watcher registered")
	}
}

func TestServeSubscriptionErrors(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "UNWIND [1, 2, 3] AS i CREATE (:N {i: i})")

	call := func(req wire.Request) error {
		ctx, cancel := context.WithTimeout(context.Background(), subTimeout)
		defer cancel()
		sink := newFakeSink(ctx)
		close(sink.acks) // acknowledge everything at once
		return db.serveSubscription(ctx, wire.SubscribeRequest{Request: req}, sink)
	}
	code := func(err error) wire.Code {
		var remote *wire.Error
		if errors.As(err, &remote) {
			return remote.Code
		}
		return wire.CodeGeneric
	}

	if err := call(wire.Request{Cypher: "MATCH (n RETURN n"}); err == nil {
		t.Error("syntax error: want an error")
	}
	if err := call(wire.Request{Cypher: "CREATE (n) RETURN n"}); err == nil {
		t.Error("writing query: want an error")
	}
	if err := call(wire.Request{Cypher: "MATCH (n:N) WHERE n.i = $missing RETURN n"}); err == nil || code(err) != wire.CodeGeneric {
		t.Errorf("missing parameter: err = %v, want a generic error", err)
	}
	if err := call(wire.Request{Cypher: "MATCH (n:N) RETURN n.i AS i", MaxRows: 2}); code(err) != wire.CodeResultLimit {
		t.Errorf("initial result over the cap: err = %v, want CodeResultLimit", err)
	}
	if err := call(wire.Request{Cypher: "MATCH (n) RETURN n", Params: []byte{0xff}}); err == nil {
		t.Error("malformed parameters: want an error")
	}
	if db.hub.Active() {
		t.Error("a failed served live query left its watcher registered")
	}

	// A result that outgrows its cap ends the stream with the limit code.
	ctx, cancel := context.WithTimeout(context.Background(), subTimeout)
	defer cancel()
	sink := newFakeSink(ctx)
	done := make(chan error, 1)
	go func() {
		done <- db.serveSubscription(ctx, wire.SubscribeRequest{Request: wire.Request{
			Cypher: "MATCH (n:N) RETURN n.i AS i", MaxRows: 3}}, sink)
	}()
	sink.next(t)
	sink.acks <- struct{}{}
	mustExec(t, db, "CREATE (:N {i: 4})")
	if err := <-done; code(err) != wire.CodeResultLimit {
		t.Errorf("growing past the cap: err = %v, want CodeResultLimit", err)
	}

	// A closed database cannot serve.
	closed, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	err = closed.serveSubscription(context.Background(), wire.SubscribeRequest{Request: wire.Request{Cypher: "MATCH (n) RETURN n"}}, newFakeSink(context.Background()))
	if code(err) != wire.CodeUnavailable {
		t.Errorf("closed database: err = %v, want CodeUnavailable", err)
	}
}

// TestServeSubscriptionWaitsForIndex covers the monotonic hand-over: a member
// asked to start after an index it has not applied waits for it, and gives up
// with CodeBehind if it never arrives.
func TestServeSubscriptionWaitsForIndex(t *testing.T) {
	old := catchUpTimeout
	catchUpTimeout = 300 * time.Millisecond
	defer func() { catchUpTimeout = old }()

	db := newDB(t)
	setApplied(t, db, 5)
	const q = "MATCH (n) RETURN n"

	// Already there: no wait.
	sink, cancel, _ := serve(t, db, wire.SubscribeRequest{Request: wire.Request{Cypher: q}, AfterIndex: 5})
	if b := sink.next(t); !b.Initial || b.Index != 5 {
		t.Fatalf("at the index: batch = %+v", b)
	}
	cancel()

	// Never gets there: CodeBehind.
	ctx, stop := context.WithTimeout(context.Background(), subTimeout)
	defer stop()
	start := time.Now()
	err := db.serveSubscription(ctx, wire.SubscribeRequest{Request: wire.Request{Cypher: q}, AfterIndex: 9}, newFakeSink(ctx))
	var remote *wire.Error
	if !errors.As(err, &remote) || remote.Code != wire.CodeBehind {
		t.Fatalf("behind: err = %v, want CodeBehind", err)
	}
	if elapsed := time.Since(start); elapsed < catchUpTimeout || elapsed > 3*time.Second {
		t.Errorf("gave up after %v, want about %v", elapsed, catchUpTimeout)
	}
	if db.hub.Active() {
		t.Error("a member that gave up left a watcher registered")
	}

	// Gets there while waiting: served, and from a state at least that new.
	catchUpTimeout = subTimeout
	sink, _, _ = serve(t, db, wire.SubscribeRequest{Request: wire.Request{Cypher: q}, AfterIndex: 9})
	select {
	case b := <-sink.batches:
		t.Fatalf("answered before reaching the index: %+v", b)
	case <-time.After(100 * time.Millisecond):
	}
	mustExec(t, db, "CREATE (:Late)")
	setApplied(t, db, 9)
	b := sink.next(t)
	if !b.Initial || b.Index != 9 || len(b.Added) != 1 {
		t.Fatalf("after catching up: batch = %+v", b)
	}
}
