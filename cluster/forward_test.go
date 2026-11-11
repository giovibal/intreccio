package cluster

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
	"github.com/giovibal/intreccio/internal/wire"
)

// TestForwardedRowCap checks that WithMaxRows reaches the member that executes
// a forwarded read, and that the limit error is still recognisable.
func TestForwardedRowCap(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	follower := tc.aFollower(t)
	ctx := context.Background()
	run(t, tc.dbs[leader], "UNWIND [1, 2, 3, 4, 5] AS i CREATE (:N {i: i})")

	const q = "MATCH (n:N) RETURN n.i AS i"
	for name, db := range map[string]*intreccio.DB{
		"follower": tc.dbs[follower],
		"client":   tc.openClient(t, "client-cap"),
	} {
		_, err := db.Query(ctx, q, nil, intreccio.Linearizable(), intreccio.WithMaxRows(3))
		if !errors.Is(err, intreccio.ErrResultLimit) {
			t.Errorf("%s: over the cap: err = %v, want ErrResultLimit", name, err)
		}
		res, err := db.Query(ctx, q, nil, intreccio.Linearizable(), intreccio.WithMaxRows(5))
		if err != nil || len(res.Rows) != 5 {
			t.Errorf("%s: at the cap: %v rows, err %v", name, res, err)
		}
	}

	// A forwarded write is never capped, exactly as a local one.
	res, err := tc.dbs[follower].Query(ctx, "MATCH (n:N) SET n.seen = true RETURN n.i AS i", nil, intreccio.WithMaxRows(2))
	if err != nil || len(res.Rows) != 5 {
		t.Errorf("forwarded write with a cap: %v rows, err %v", res, err)
	}
}

// TestForwardedParams checks that parameters of every supported shape —
// including nulls nested in lists and maps — survive forwarding.
func TestForwardedParams(t *testing.T) {
	tc := startCluster(t, 3)
	tc.leaderIndex(t)
	params := map[string]any{
		"s":    "text",
		"i":    42,
		"f":    1.5,
		"b":    true,
		"null": nil,
		"list": []any{int64(1), nil, "x", []any{nil}},
		"map":  map[string]any{"k": nil, "n": int64(2), "inner": map[string]any{"deep": []any{true}}},
	}
	want := [][]any{{
		"text", int64(42), 1.5, true, nil,
		[]any{int64(1), nil, "x", []any{nil}},
		map[string]any{"k": nil, "n": int64(2), "inner": map[string]any{"deep": []any{true}}},
	}}
	const q = "RETURN $s AS s, $i AS i, $f AS f, $b AS b, $null AS n, $list AS l, $map AS m"
	for name, db := range map[string]*intreccio.DB{
		"follower": tc.dbs[tc.aFollower(t)],
		"client":   tc.openClient(t, "client-params"),
	} {
		res, err := db.Query(context.Background(), q, params, intreccio.Linearizable())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(res.Rows, want) {
			t.Errorf("%s: rows = %#v, want %#v", name, res.Rows, want)
		}
	}

	// Go-typed parameters are normalised before they are sent...
	res, err := tc.dbs[tc.aFollower(t)].Query(context.Background(), "RETURN $p AS p",
		map[string]any{"p": []string{"a", "b"}}, intreccio.Linearizable())
	if err != nil || !reflect.DeepEqual(res.Rows, [][]any{{[]any{"a", "b"}}}) {
		t.Errorf("a []string parameter: rows = %v, err = %v", res, err)
	}
	// ...and one that is not a value at all is refused before anything is sent.
	_, err = tc.dbs[tc.aFollower(t)].Query(context.Background(), "RETURN $p AS p",
		map[string]any{"p": make(chan int)}, intreccio.Linearizable())
	if err == nil {
		t.Error("an unsupported parameter type was forwarded")
	}
}

// TestCancellationReachesTheExecutingNode checks the whole path: cancelling a
// query on a follower, or on a dataless client whose request is proxied by a
// voter, stops the execution on the leader.
func TestCancellationReachesTheExecutingNode(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)

	// Replace the leader's executor with one that blocks until it is told to
	// stop, and reports how it was stopped.
	started := make(chan struct{}, 4)
	stopped := make(chan error, 4)
	real := tc.nodes[leader].localExec.Load()
	tc.nodes[leader].SetLocalExecutor(func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		if req.Cypher != "RETURN 'block' AS x" {
			return (*real)(ctx, req)
		}
		started <- struct{}{}
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, nil, ctx.Err()
	})

	check := func(name string, db *intreccio.DB, stop func(context.Context) (context.Context, context.CancelFunc), want error) {
		t.Helper()
		ctx, cancel := stop(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := db.Query(ctx, "RETURN 'block' AS x", nil, intreccio.Linearizable())
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the request never reached the leader", name)
		}
		if want == context.Canceled {
			cancel()
		}
		select {
		case err := <-done:
			if !errors.Is(err, want) {
				t.Errorf("%s: query error = %v, want %v", name, err, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the query did not return", name)
		}
		select {
		case err := <-stopped:
			if err == nil {
				t.Errorf("%s: the execution on the leader stopped without a context error", name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the execution on the leader was never stopped", name)
		}
	}

	withCancel := func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	withDeadline := func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 300*time.Millisecond)
	}
	check("follower cancel", tc.dbs[tc.aFollower(t)], withCancel, context.Canceled)
	check("follower deadline", tc.dbs[tc.aFollower(t)], withDeadline, context.DeadlineExceeded)
	check("client cancel", tc.openClient(t, "client-cancel"), withCancel, context.Canceled)
}

// TestForwardedStreamQueryIsRefused checks that a member never runs a live
// query as a plain forwarded request.
func TestForwardedStreamQueryIsRefused(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	pool := newTestPool(t)
	_, _, err := pool.call(context.Background(), tc.peers[leader].ForwardAddr,
		wire.Request{Cypher: "STREAM MATCH (n) RETURN n"})
	if err == nil {
		t.Fatal("a STREAM query was executed as a forwarded request")
	}
}
