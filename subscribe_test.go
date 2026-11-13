package intreccio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

const subTimeout = 5 * time.Second

func subscribe(t *testing.T, db *DB, cypher string, params map[string]any, opts ...SubscribeOption) *Subscription {
	t.Helper()
	sub, err := db.Subscribe(context.Background(), cypher, params, opts...)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", cypher, err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// nextChange waits for the next batch.
func nextChange(t *testing.T, sub *Subscription) Change {
	t.Helper()
	select {
	case c, ok := <-sub.Changes():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		return c
	case <-time.After(subTimeout):
		t.Fatal("timed out waiting for a change")
	}
	return Change{}
}

// expectQuiet asserts that no batch arrives for a short while.
func expectQuiet(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case c, ok := <-sub.Changes():
		if ok {
			t.Fatalf("unexpected change: %+v", c)
		}
		t.Fatalf("subscription ended: %v", sub.Err())
	case <-time.After(150 * time.Millisecond):
	}
}

// expectEnd waits for the subscription to end and returns its error.
func expectEnd(t *testing.T, sub *Subscription) error {
	t.Helper()
	deadline := time.After(subTimeout)
	for {
		select {
		case _, ok := <-sub.Changes():
			if !ok {
				return sub.Err()
			}
		case <-deadline:
			t.Fatal("timed out waiting for the subscription to end")
		}
	}
}

func mustExec(t *testing.T, db *DB, cypher string, params ...map[string]any) {
	t.Helper()
	var p map[string]any
	if len(params) > 0 {
		p = params[0]
	}
	if _, err := db.Query(context.Background(), cypher, p); err != nil {
		t.Fatalf("%s: %v", cypher, err)
	}
}

func oneCol(vals ...any) [][]any {
	out := make([][]any, len(vals))
	for i, v := range vals {
		out[i] = []any{v}
	}
	return out
}

func wantChange(t *testing.T, got Change, added, removed [][]any) {
	t.Helper()
	if got.Initial {
		t.Errorf("change flagged as initial: %+v", got)
	}
	if len(got.Added) != len(added) || (len(added) > 0 && !reflect.DeepEqual(got.Added, added)) {
		t.Errorf("Added = %v, want %v", got.Added, added)
	}
	if len(got.Removed) != len(removed) || (len(removed) > 0 && !reflect.DeepEqual(got.Removed, removed)) {
		t.Errorf("Removed = %v, want %v", got.Removed, removed)
	}
}

func TestSubscribeInitialThenChanges(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'Bob'}), (:Person {name: 'Ann'}), (:Company {name: 'Acme'})")

	const q = "MATCH (p:Person) RETURN p.name AS name ORDER BY name"
	sub := subscribe(t, db, q, nil)
	if got := sub.Columns(); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("Columns = %v", got)
	}

	// The first batch is the current result, exactly as Query returns it.
	first := nextChange(t, sub)
	res, err := db.Query(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Initial || len(first.Removed) != 0 || !reflect.DeepEqual(first.Added, res.Rows) {
		t.Fatalf("initial = %+v, want Added=%v", first, res.Rows)
	}

	mustExec(t, db, "CREATE (:Person {name: 'Cy'})")
	wantChange(t, nextChange(t, sub), oneCol("Cy"), nil)

	// An update is the old row removed and the new row added, in one batch.
	mustExec(t, db, "MATCH (p:Person {name: 'Bob'}) SET p.name = 'Rob'")
	wantChange(t, nextChange(t, sub), oneCol("Rob"), oneCol("Bob"))

	mustExec(t, db, "MATCH (p:Person {name: 'Ann'}) DELETE p")
	wantChange(t, nextChange(t, sub), nil, oneCol("Ann"))

	// A row leaves the result when it stops matching, not only when deleted.
	mustExec(t, db, "MATCH (p:Person {name: 'Cy'}) REMOVE p:Person")
	wantChange(t, nextChange(t, sub), nil, oneCol("Cy"))
}

func TestSubscribeIgnoresIrrelevantWrites(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'Ann', age: 1})")
	sub := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	nextChange(t, sub)

	mustExec(t, db, "CREATE (:Company {name: 'Acme'})")                          // other label
	mustExec(t, db, "MATCH (p:Person) SET p.age = 2")                            // unread property
	mustExec(t, db, "MATCH (p:Person) SET p.name = 'Ann'")                       // same value
	mustExec(t, db, "MATCH (p:Person), (c:Company) CREATE (p)-[:WORKS_AT]->(c)") // no relationship in the query
	expectQuiet(t, sub)

	// A relevant write whose net effect on the result is nil emits nothing.
	mustExec(t, db, "CREATE (:Person {name: 'Tmp'})")
	wantChange(t, nextChange(t, sub), oneCol("Tmp"), nil)
}

func TestSubscribeEmptyInitial(t *testing.T) {
	db := newDB(t)
	sub := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	first := nextChange(t, sub)
	if !first.Initial || len(first.Added) != 0 || len(first.Removed) != 0 {
		t.Fatalf("initial = %+v, want an empty initial batch", first)
	}
	mustExec(t, db, "CREATE (:Person {name: 'Ann'})")
	wantChange(t, nextChange(t, sub), oneCol("Ann"), nil)
}

func TestSubscribeWholeEntitiesAndRelationships(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'Ann'}), (:Person {name: 'Bob'})")
	sub := subscribe(t, db, "MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a, r, b.name AS friend", nil)
	if first := nextChange(t, sub); len(first.Added) != 0 {
		t.Fatalf("initial = %+v, want empty", first)
	}

	mustExec(t, db, "MATCH (a:Person {name: 'Ann'}), (b:Person {name: 'Bob'}) CREATE (a)-[:KNOWS {since: 2020}]->(b)")
	c := nextChange(t, sub)
	if len(c.Added) != 1 || len(c.Removed) != 0 {
		t.Fatalf("after create relationship: %+v", c)
	}
	a, okA := c.Added[0][0].(Node)
	r, okR := c.Added[0][1].(Relationship)
	if !okA || !okR || a.Props["name"] != "Ann" || r.Type != "KNOWS" || c.Added[0][2] != "Bob" {
		t.Fatalf("row = %#v", c.Added[0])
	}

	// A property the query only sees through the whole node still matters.
	mustExec(t, db, "MATCH (a:Person {name: 'Ann'}) SET a.age = 30")
	c = nextChange(t, sub)
	if len(c.Added) != 1 || len(c.Removed) != 1 {
		t.Fatalf("after update: %+v", c)
	}
	if old, cur := c.Removed[0][0].(Node), c.Added[0][0].(Node); old.Props["age"] != nil || cur.Props["age"] != int64(30) || old.ID != cur.ID {
		t.Errorf("removed %+v / added %+v", old, cur)
	}

	mustExec(t, db, "MATCH (:Person)-[r:KNOWS]->() DELETE r")
	c = nextChange(t, sub)
	if len(c.Added) != 0 || len(c.Removed) != 1 {
		t.Fatalf("after delete relationship: %+v", c)
	}
}

func TestSubscribeAggregationAndTopN(t *testing.T) {
	db := newDB(t)
	count := subscribe(t, db, "MATCH (p:Person) RETURN count(p) AS c", nil)
	top := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name, p.score AS score ORDER BY score DESC LIMIT 2", nil)
	if first := nextChange(t, count); !reflect.DeepEqual(first.Added, oneCol(int64(0))) {
		t.Fatalf("count initial = %+v", first)
	}
	nextChange(t, top)

	mustExec(t, db, "CREATE (:Person {name: 'a', score: 1}), (:Person {name: 'b', score: 2})")
	wantChange(t, nextChange(t, count), oneCol(int64(2)), oneCol(int64(0)))
	wantChange(t, nextChange(t, top), [][]any{{"b", int64(2)}, {"a", int64(1)}}, nil)

	// A new leader pushes the lowest of the top two out.
	mustExec(t, db, "CREATE (:Person {name: 'c', score: 3})")
	wantChange(t, nextChange(t, count), oneCol(int64(3)), oneCol(int64(2)))
	wantChange(t, nextChange(t, top), [][]any{{"c", int64(3)}}, [][]any{{"a", int64(1)}})

	// Below the cut: the count moves, the top two do not.
	mustExec(t, db, "CREATE (:Person {name: 'd', score: 0})")
	wantChange(t, nextChange(t, count), oneCol(int64(4)), oneCol(int64(3)))
	expectQuiet(t, top)
}

func TestSubscribeParamsAndStreamPrefix(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:Person {name: 'Ann', city: 'Rome'}), (:Person {name: 'Bob', city: 'Oslo'})")
	sub := subscribe(t, db, "STREAM MATCH (p:Person) WHERE p.city = $city RETURN p.name AS name",
		map[string]any{"city": "Rome"})
	if first := nextChange(t, sub); !reflect.DeepEqual(first.Added, oneCol("Ann")) {
		t.Fatalf("initial = %+v", first)
	}
	mustExec(t, db, "MATCH (p:Person {name: 'Bob'}) SET p.city = 'Rome'")
	wantChange(t, nextChange(t, sub), oneCol("Bob"), nil)
}

func TestStreamQueryNeedsSubscribe(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := db.Query(ctx, "STREAM MATCH (n) RETURN n", nil); !errors.Is(err, ErrStreamQuery) {
		t.Errorf("Query(STREAM ...) error = %v, want ErrStreamQuery", err)
	}
	// Explain still works on a live query.
	if _, err := db.Explain(ctx, "STREAM MATCH (n:Person) RETURN n"); err != nil {
		t.Errorf("Explain(STREAM ...): %v", err)
	}
}

func TestSubscribeRejectsBadQueries(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustExec(t, db, "CREATE (:Person {x: 1})")
	for _, q := range []string{
		"MATCH (n RETURN n",                       // syntax
		"MATCH (n) RETURN m",                      // semantics
		"CREATE (n:Person) RETURN n",              // write
		"STREAM MATCH (n) SET n.x = 1 RETURN n",   // write, flagged
		"MATCH (n) WHERE n.x = $missing RETURN n", // fails on first evaluation
	} {
		if sub, err := db.Subscribe(ctx, q, nil); err == nil {
			_ = sub.Close()
			t.Errorf("Subscribe(%q): expected an error", q)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := db.Subscribe(canceled, "MATCH (n) RETURN n", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe with a canceled context: %v", err)
	}
	if db.hub.Active() {
		t.Error("a failed Subscribe left a watcher registered")
	}
}

func TestSubscribeRowCap(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "UNWIND [1, 2, 3] AS i CREATE (:N {i: i})")

	if _, err := db.Subscribe(context.Background(), "MATCH (n:N) RETURN n.i AS i", nil,
		WithSubscriptionMaxRows(2)); !errors.Is(err, ErrResultLimit) {
		t.Fatalf("initial result over the cap: err = %v, want ErrResultLimit", err)
	}

	sub := subscribe(t, db, "MATCH (n:N) RETURN n.i AS i", nil, WithSubscriptionMaxRows(4))
	nextChange(t, sub)
	mustExec(t, db, "CREATE (:N {i: 4})")
	if c := nextChange(t, sub); len(c.Added) != 1 {
		t.Fatalf("at the cap: %+v", c)
	}
	mustExec(t, db, "CREATE (:N {i: 5})")
	if err := expectEnd(t, sub); !errors.Is(err, ErrResultLimit) {
		t.Errorf("growing past the cap: Err = %v, want ErrResultLimit", err)
	}

	// A non-positive cap disables the limit.
	unlimited := subscribe(t, db, "MATCH (n:N) RETURN n.i AS i", nil, WithSubscriptionMaxRows(0))
	if c := nextChange(t, unlimited); len(c.Added) != 5 {
		t.Errorf("unlimited initial = %d rows, want 5", len(c.Added))
	}
}

func TestSubscriptionEndings(t *testing.T) {
	db := newDB(t)

	t.Run("close", func(t *testing.T) {
		sub := subscribe(t, db, "MATCH (n) RETURN n", nil)
		if sub.Err() != nil {
			t.Errorf("Err on a live subscription = %v", sub.Err())
		}
		if err := sub.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := sub.Close(); err != nil { // idempotent
			t.Errorf("second Close: %v", err)
		}
		if err := expectEnd(t, sub); err != nil {
			t.Errorf("Err after Close = %v, want nil", err)
		}
	})

	t.Run("close without ever reading", func(t *testing.T) {
		sub := subscribe(t, db, "MATCH (n) RETURN n", nil)
		done := make(chan struct{})
		go func() { _ = sub.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(subTimeout):
			t.Fatal("Close blocked on an unread subscription")
		}
	})

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		sub, err := db.Subscribe(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatal(err)
		}
		nextChange(t, sub)
		cancel()
		if err := expectEnd(t, sub); !errors.Is(err, context.Canceled) {
			t.Errorf("Err after cancel = %v, want context.Canceled", err)
		}
	})

	t.Run("context with a custom cause", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		sub, err := db.Subscribe(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel(errors.New("interrupt signal received"))
		if err := expectEnd(t, sub); !errors.Is(err, context.Canceled) {
			t.Errorf("Err after cancel with a cause = %v, want context.Canceled", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		sub, err := db.Subscribe(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := expectEnd(t, sub); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Err after the deadline = %v, want context.DeadlineExceeded", err)
		}
	})

	if db.hub.Active() {
		t.Error("ended subscriptions left watchers registered")
	}
}

func TestDatabaseCloseEndsSubscriptions(t *testing.T) {
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.Subscribe(context.Background(), "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.Subscribe(context.Background(), "MATCH (n:X) RETURN n.k AS k", nil)
	if err != nil {
		t.Fatal(err)
	}
	nextChange(t, a) // b is never read

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, sub := range []*Subscription{a, b} {
		if err := expectEnd(t, sub); !errors.Is(err, ErrClosed) {
			t.Errorf("Err after database close = %v, want ErrClosed", err)
		}
	}
	if _, err := db.Subscribe(context.Background(), "MATCH (n) RETURN n", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe on a closed database: %v, want ErrClosed", err)
	}
}

// model replays batches into a multiset of rows, as a consumer would.
type model map[string]int

func (m model) apply(t *testing.T, c Change) {
	t.Helper()
	for _, row := range c.Removed {
		k := fmt.Sprint(row)
		if m[k] == 0 {
			t.Errorf("row %v removed but never added", row)
			continue
		}
		if m[k]--; m[k] == 0 {
			delete(m, k)
		}
	}
	for _, row := range c.Added {
		m[fmt.Sprint(row)]++
	}
}

func modelOf(rows [][]any) model {
	m := model{}
	for _, row := range rows {
		m[fmt.Sprint(row)]++
	}
	return m
}

// TestSubscribeCoalesces checks that a consumer that does not read gets the net
// difference in a couple of batches, not one batch per commit.
func TestSubscribeCoalesces(t *testing.T) {
	db := newDB(t)
	const q = "MATCH (n:N) RETURN n.i AS i"
	sub := subscribe(t, db, q, nil)

	const writes = 60
	for i := range writes {
		mustExec(t, db, "CREATE (:N {i: $i})", map[string]any{"i": i})
		if i%3 == 0 {
			mustExec(t, db, "MATCH (n:N {i: $i}) DELETE n", map[string]any{"i": i})
		}
	}
	res, err := db.Query(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := modelOf(res.Rows)

	got, batches := model{}, 0
	for !reflect.DeepEqual(got, want) {
		got.apply(t, nextChange(t, sub))
		batches++
	}
	expectQuiet(t, sub)
	if batches > 3 {
		t.Errorf("%d batches for %d unread commits: wake-ups are not coalescing", batches, writes)
	}
}

// TestSubscribeConvergesUnderConcurrentWrites runs writers and subscribers
// together (meaningful under -race) and checks that every subscriber converges
// on the final result.
func TestSubscribeConvergesUnderConcurrentWrites(t *testing.T) {
	db := newDB(t)
	queries := []string{
		"MATCH (n:N) RETURN n.w AS w, n.i AS i",
		"MATCH (n:N) WHERE n.i >= 10 RETURN n",
		"MATCH (n:N) RETURN n.w AS w, count(n) AS c",
		"MATCH (a:N)-[:NEXT]->(b:N) RETURN a.i AS a, b.i AS b",
	}
	subs := make([]*Subscription, len(queries))
	for i, q := range queries {
		subs[i] = subscribe(t, db, q, nil)
	}

	var wg sync.WaitGroup
	for w := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// t.Fatal must not be called off the test goroutine: report and stop.
			exec := func(cypher string, p map[string]any) bool {
				if _, err := db.Query(context.Background(), cypher, p); err != nil {
					t.Errorf("%s: %v", cypher, err)
					return false
				}
				return true
			}
			for i := range 10 {
				p := map[string]any{"w": w, "i": i}
				ok := exec("CREATE (:N {w: $w, i: $i})", p)
				if ok && i > 0 {
					ok = exec("MATCH (a:N {w: $w, i: $i}), (b:N {w: $w}) WHERE b.i = $i - 1 CREATE (b)-[:NEXT]->(a)", p)
				}
				if ok && i%5 == 4 {
					ok = exec("MATCH (n:N {w: $w, i: $i}) DETACH DELETE n", p)
				}
				if ok && i%7 == 6 {
					ok = exec("MATCH (n:N {w: $w, i: $i}) SET n.i = $i + 100", p)
				}
				if !ok {
					return
				}
			}
		}()
	}
	wg.Wait()

	for i, q := range queries {
		res, err := db.Query(context.Background(), q, nil)
		if err != nil {
			t.Fatal(err)
		}
		want, got := modelOf(res.Rows), model{}
		deadline := time.After(subTimeout)
		for !reflect.DeepEqual(got, want) {
			select {
			case c, ok := <-subs[i].Changes():
				if !ok {
					t.Fatalf("%s: subscription ended: %v", q, subs[i].Err())
				}
				got.apply(t, c)
			case <-deadline:
				keys := func(m model) []string {
					out := make([]string, 0, len(m))
					for k := range m {
						out = append(out, k)
					}
					sort.Strings(out)
					return out
				}
				t.Fatalf("%s: did not converge\n got: %v\nwant: %v", q, keys(got), keys(want))
			}
		}
	}
}

// TestSubscribeNoGoroutineLeak checks that ended subscriptions leave no
// goroutine behind, however they ended.
func TestSubscribeNoGoroutineLeak(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE (:N {i: 1})")
	before := runtime.NumGoroutine()

	for i := range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		sub, err := db.Subscribe(ctx, "MATCH (n:N) RETURN n.i AS i", nil)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			nextChange(t, sub)
		}
		mustExec(t, db, "MATCH (n:N) SET n.i = n.i + 1")
		if i%3 == 0 {
			cancel()
			_ = expectEnd(t, sub)
		} else {
			_ = sub.Close()
			cancel()
		}
	}

	deadline := time.Now().Add(subTimeout)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if db.hub.Active() {
		t.Error("watchers left registered")
	}
}

func TestSubscribeOnPersistentDatabase(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	sub := subscribe(t, db, "MATCH (p:Person) RETURN p.name AS name", nil)
	nextChange(t, sub)
	mustExec(t, db, "CREATE (:Person {name: 'Ann'})")
	wantChange(t, nextChange(t, sub), oneCol("Ann"), nil)
}

// TestErrIsSetWhenChangesCloses checks the contract of Changes: once the
// channel is closed, Err already tells why. Changes used to be closed before
// the reason was published, so a consumer reading Err right away could get
// nil.
func TestErrIsSetWhenChangesCloses(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		sub, err := db.Subscribe(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		for range sub.Changes() {
		}
		if err := sub.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d: Err right after Changes closed = %v, want context.Canceled", i, err)
		}
	}
}
