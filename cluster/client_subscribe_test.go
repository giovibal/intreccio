package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

// servingVoter returns the index of the voter currently evaluating a live
// query for someone, waiting for exactly one to be.
func (tc *testCluster) servingVoter(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for {
		var serving []int
		for i, nd := range tc.nodes {
			if nd != nil && nd.fsm.hub.Load().Active() {
				serving = append(serving, i)
			}
		}
		if len(serving) == 1 {
			return serving[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("voters serving a live query: %v, want exactly one", serving)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// noneServing waits until no voter is evaluating a live query.
func (tc *testCluster) noneServing(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for {
		busy := false
		for _, nd := range tc.nodes {
			if nd != nil && nd.fsm.hub.Load().Active() {
				busy = true
			}
		}
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a voter is still evaluating a live query nobody listens to")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (tc *testCluster) kill(t *testing.T, i int) {
	t.Helper()
	if err := tc.nodes[i].Close(); err != nil {
		t.Fatalf("close voter %d: %v", i, err)
	}
	tc.nodes[i] = nil
}

func endOf(t *testing.T, sub *intreccio.Subscription) error {
	t.Helper()
	deadline := time.After(liveTimeout)
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

// TestClientSubscribe verifies that a dataless client can run a live query: a
// voter evaluates it and the client receives the same batches a local
// subscriber would.
func TestClientSubscribe(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Ann'})")
	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Ann"})
	}
	clientDB := tc.openClient(t, "client-live")

	sub := subscribeTo(t, clientDB, "STREAM "+liveNames)
	if cols := sub.Columns(); !reflect.DeepEqual(cols, []string{"name"}) {
		t.Errorf("Columns = %v", cols)
	}
	first := nextBatch(t, sub)
	if !first.Initial || !reflect.DeepEqual(first.Added, [][]any{{"Ann"}}) || len(first.Removed) != 0 {
		t.Fatalf("initial = %+v", first)
	}
	tc.servingVoter(t)

	// A write made through the client itself comes back as a change.
	run(t, clientDB, "CREATE (:Person {name: 'Bob'})")
	if c := nextBatch(t, sub); c.Initial || !reflect.DeepEqual(c.Added, [][]any{{"Bob"}}) || len(c.Removed) != 0 {
		t.Fatalf("after create = %+v", c)
	}
	run(t, tc.dbs[leader], "MATCH (p:Person {name: 'Bob'}) SET p.name = 'Rob'")
	if c := nextBatch(t, sub); !reflect.DeepEqual(c.Added, [][]any{{"Rob"}}) || !reflect.DeepEqual(c.Removed, [][]any{{"Bob"}}) {
		t.Fatalf("after update = %+v", c)
	}
	run(t, tc.dbs[leader], "CREATE (:Company {name: 'Acme'})")
	noBatch(t, sub)

	// Whole entities travel too.
	nodes := subscribeTo(t, clientDB, "MATCH (p:Person {name: 'Ann'}) RETURN p")
	c := nextBatch(t, nodes)
	if p, ok := c.Added[0][0].(intreccio.Node); !ok || p.Props["name"] != "Ann" || !reflect.DeepEqual(p.Labels, []string{"Person"}) {
		t.Fatalf("node row = %#v", c.Added[0][0])
	}
	_ = nodes.Close()

	// Closing the subscription stops the voter's work for it.
	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := endOf(t, sub); err != nil {
		t.Errorf("Err after Close = %v, want nil", err)
	}
	tc.noneServing(t)
}

// TestClientSubscribeErrors covers what a client's Subscribe and subscription
// report when the query itself is the problem: the first voter's answer is
// final, and no voter is left working.
func TestClientSubscribeErrors(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	run(t, tc.dbs[leader], "UNWIND [1, 2, 3] AS i CREATE (:N {i: i})")
	for i := range tc.nodes {
		waitForCountN(t, tc.dbs[i], 3)
	}
	clientDB := tc.openClient(t, "client-errors")
	ctx := context.Background()

	if _, err := clientDB.Subscribe(ctx, "CREATE (n) RETURN n", nil); err == nil {
		t.Error("writing query: want an error")
	}
	if _, err := clientDB.Subscribe(ctx, "MATCH (n:N) WHERE n.i = $missing RETURN n", nil); err == nil ||
		!strings.Contains(err.Error(), "missing") {
		t.Errorf("missing parameter: err = %v", err)
	}
	if _, err := clientDB.Subscribe(ctx, "MATCH (n:N) RETURN n.i AS i", nil,
		intreccio.WithSubscriptionMaxRows(2)); !errors.Is(err, intreccio.ErrResultLimit) {
		t.Errorf("initial result over the cap: err = %v, want ErrResultLimit", err)
	}

	sub, err := clientDB.Subscribe(ctx, "MATCH (n:N) RETURN n.i AS i", nil, intreccio.WithSubscriptionMaxRows(3))
	if err != nil {
		t.Fatal(err)
	}
	nextBatch(t, sub)
	run(t, tc.dbs[leader], "CREATE (:N {i: 4})")
	if err := endOf(t, sub); !errors.Is(err, intreccio.ErrResultLimit) {
		t.Errorf("growing past the cap: Err = %v, want ErrResultLimit", err)
	}
	tc.noneServing(t)
}

func waitForCountN(t *testing.T, db *intreccio.DB, want int64) {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for {
		res, err := db.Query(context.Background(), "MATCH (n:N) RETURN count(n) AS c", nil)
		if err == nil && len(res.Rows) == 1 && res.Rows[0][0] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("count never reached %d (last: %v, %v)", want, res, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestClientSubscriptionFailover is the transparent failover: the voter serving
// a client's live query is killed, and the application sees nothing but the
// next real change.
func TestClientSubscriptionFailover(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Ann'}), (:Person {name: 'Bob'})")
	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Ann", "Bob"})
	}
	clientDB := tc.openClient(t, "client-failover")

	sub := subscribeTo(t, clientDB, liveNames)
	if first := nextBatch(t, sub); !first.Initial || len(first.Added) != 2 {
		t.Fatalf("initial = %+v", first)
	}

	serving := tc.servingVoter(t)
	tc.kill(t, serving)
	// The subscription moves to another voter by itself.
	if moved := tc.servingVoter(t); moved == serving {
		t.Fatal("the subscription did not move to another voter")
	}
	// Nothing changed in the result: the application must hear nothing.
	noBatch(t, sub)

	run(t, tc.dbs[tc.leaderIndex(t)], "CREATE (:Person {name: 'Cy'})")
	c := nextBatch(t, sub)
	if c.Initial || !reflect.DeepEqual(c.Added, [][]any{{"Cy"}}) || len(c.Removed) != 0 {
		t.Fatalf("after failover the application got %+v, want only the new row", c)
	}
	noBatch(t, sub)
	if sub.Err() != nil {
		t.Errorf("Err on a live subscription = %v", sub.Err())
	}
}

// TestClientSubscriptionFailoverCatchesUp changes the result while the client
// is between voters: it must end up with exactly the net difference.
func TestClientSubscriptionFailoverCatchesUp(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	run(t, tc.dbs[leader], "CREATE (:Person {name: 'Ann'}), (:Person {name: 'Bob'})")
	for i := range tc.nodes {
		waitForLocal(t, tc.dbs[i], []string{"Ann", "Bob"})
	}
	clientDB := tc.openClient(t, "client-catchup")
	live := follow(subscribeTo(t, clientDB, liveNames))
	live.until(t, "Ann", "Bob")

	serving := tc.servingVoter(t)
	tc.kill(t, serving)
	// Write straight away, racing the hand-over to another voter.
	db := tc.dbs[tc.leaderIndex(t)]
	run(t, db, "MATCH (p:Person {name: 'Ann'}) DELETE p")
	run(t, db, "CREATE (:Person {name: 'Cy'})")
	run(t, db, "MATCH (p:Person {name: 'Bob'}) SET p.name = 'Rob'")
	live.until(t, "Cy", "Rob")
	noBatch(t, live.sub)

	// And again, with the one remaining failover the quorum allows... a second
	// loss would stop writes, so only check the stream is still healthy.
	run(t, db, "CREATE (:Person {name: 'Dee'})")
	live.until(t, "Cy", "Dee", "Rob")
}

// TestClientSubscriptionEndsWhenNoVoterIsLeft checks the bound on failover: with
// every voter gone the subscription ends with an error instead of hanging.
func TestClientSubscriptionEndsWhenNoVoterIsLeft(t *testing.T) {
	tc := startCluster(t, 3)
	tc.leaderIndex(t)
	c, err := newClient(Config{NodeID: "client-lost", Role: RoleClient, Peers: tc.peers})
	if err != nil {
		t.Fatal(err)
	}
	c.failoverTimeout = 500 * time.Millisecond
	clientDB := wrap(c)
	defer func() { _ = clientDB.Close() }()

	sub := subscribeTo(t, clientDB, liveNames)
	nextBatch(t, sub)
	for i := range tc.nodes {
		tc.kill(t, i)
	}
	err = endOf(t, sub)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Err with no voter left = %v, want a failover error", err)
	}
}

// TestClientSubscriptionCoalescesAcrossTheNetwork checks the flow control: a
// client that does not read holds the query on the voter instead of receiving
// a batch per commit, and its other requests are not held up meanwhile.
func TestClientSubscriptionCoalescesAcrossTheNetwork(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	clientDB := tc.openClient(t, "client-slow")
	sub := subscribeTo(t, clientDB, "MATCH (n:N) RETURN n.i AS i")

	const writes = 40
	for i := range writes {
		if _, err := tc.dbs[leader].Query(context.Background(), "CREATE (:N {i: $i})", map[string]any{"i": i}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// The unread subscription shares the client's connections: queries still work.
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()
	for range 5 {
		res, err := clientDB.Query(ctx, "MATCH (n:N) RETURN count(n) AS c", nil, intreccio.Linearizable())
		if err != nil || res.Rows[0][0] != int64(writes) {
			t.Fatalf("query beside an unread subscription: %v, %v", res, err)
		}
	}

	seen, batches := map[any]bool{}, 0
	for len(seen) < writes {
		c := nextBatch(t, sub)
		batches++
		for _, row := range c.Added {
			seen[row[0]] = true
		}
	}
	if batches > 4 {
		t.Errorf("%d batches for %d unread commits: the voter is not coalescing for a slow client", batches, writes)
	}
}

// TestClientSubscriptionLargeInitialResult sends an initial result far larger
// than one frame.
func TestClientSubscriptionLargeInitialResult(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	const n = 3000
	pad := strings.Repeat("x", 300)
	if _, err := tc.dbs[leader].Query(context.Background(),
		"UNWIND $is AS i CREATE (:Big {i: i, pad: $pad})", map[string]any{"is": ints(n), "pad": pad}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	clientDB := tc.openClient(t, "client-big")
	// Wait until whichever voter serves the client has the data.
	deadline := time.Now().Add(liveTimeout)
	for i := 0; i < len(tc.dbs); {
		res, err := tc.dbs[i].Query(context.Background(), "MATCH (b:Big) RETURN count(b) AS c", nil)
		if err == nil && res.Rows[0][0] == int64(n) {
			i++
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("voter %d never got the data", i)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sub, err := clientDB.Subscribe(context.Background(), "MATCH (b:Big) RETURN b.i AS i, b.pad AS pad", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	first := nextBatch(t, sub)
	if !first.Initial || len(first.Added) != n {
		t.Fatalf("initial batch has %d rows, want %d", len(first.Added), n)
	}
	seen := map[any]bool{}
	for _, row := range first.Added {
		if row[1] != pad {
			t.Fatal("a row arrived damaged")
		}
		seen[row[0]] = true
	}
	if len(seen) != n {
		t.Errorf("%d distinct rows, want %d", len(seen), n)
	}
}

func ints(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = int64(i)
	}
	return out
}

// TestClientCloseEndsItsSubscriptions checks both directions of a client going
// away: its subscriptions end with ErrClosed, and the voter that was serving
// them stops.
func TestClientCloseEndsItsSubscriptions(t *testing.T) {
	tc := startCluster(t, 3)
	tc.leaderIndex(t)
	clientDB, err := Open(Config{NodeID: "client-closing", Role: RoleClient, Peers: tc.peers})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := clientDB.Subscribe(context.Background(), liveNames, nil)
	if err != nil {
		t.Fatal(err)
	}
	nextBatch(t, sub)
	tc.servingVoter(t)

	if err := clientDB.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := endOf(t, sub); !errors.Is(err, intreccio.ErrClosed) {
		t.Errorf("Err after the client closed = %v, want ErrClosed", err)
	}
	tc.noneServing(t)
}
