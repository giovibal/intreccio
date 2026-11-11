package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
	"github.com/giovibal/intreccio/internal/wire"
)

// TestForwardCarriesDeadlineAndMaxRows covers #32 on the serving side: the
// executor runs under the caller's remaining deadline and WithMaxRows.
func TestForwardCarriesDeadlineAndMaxRows(t *testing.T) {
	var (
		gotDeadline bool
		gotLeft     time.Duration
		gotMax      int64
	)
	n := &raftNode{}
	n.SetLocalExecutor(func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		var d time.Time
		d, gotDeadline = ctx.Deadline()
		gotLeft = time.Until(d)
		gotMax = req.MaxRows
		return []string{"x"}, [][]byte{{1}}, nil
	})
	// The node's handler behind a real forwarding server: local reads need no Raft.
	addr, _ := startServer(t, n.serve)
	pool := newTestPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pool.call(ctx, addr, wire.Request{Cypher: "RETURN 1 AS x", MaxRows: 7}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !gotDeadline || gotLeft <= 0 || gotLeft > 5*time.Second {
		t.Errorf("executor deadline = %v (set %v), want within 5s", gotLeft, gotDeadline)
	}
	if gotMax != 7 {
		t.Errorf("executor maxRows = %d, want 7", gotMax)
	}

	// Without a caller deadline the executor gets none either.
	if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "RETURN 1 AS x"}); err != nil {
		t.Fatal(err)
	}
	if gotDeadline {
		t.Errorf("executor has a deadline, want none")
	}

	// An already expired deadline fails before anything is sent.
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if _, _, err := pool.call(expired, addr, wire.Request{Cypher: "RETURN 1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired deadline = %v, want DeadlineExceeded", err)
	}
}

// TestForwardedQueriesKeepErrorTypes covers #32 end to end: errors from the
// leader still match their sentinels on a follower and on a dataless client,
// and WithMaxRows applies to forwarded reads.
func TestForwardedQueriesKeepErrorTypes(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	lead := tc.dbs[tc.leaderIndex(t)]
	if _, err := lead.Query(ctx, "UNWIND range(1, 5) AS i CREATE (:Person {name: toString(i)})", nil); err != nil {
		t.Fatal(err)
	}
	follower := tc.dbs[tc.aFollower(t)]
	client := tc.openClient(t, "client-errs")

	for name, db := range map[string]*intreccio.DB{"follower": follower, "client": client} {
		_, err := db.Query(ctx, matchNames, nil, intreccio.Linearizable(), intreccio.WithMaxRows(2))
		if !errors.Is(err, intreccio.ErrResultLimit) {
			t.Errorf("%s: linearizable read over the limit = %v, want ErrResultLimit", name, err)
		}
		if _, err := db.Query(ctx, matchNames, nil, intreccio.Linearizable(), intreccio.WithMaxRows(5)); err != nil {
			t.Errorf("%s: read within the limit: %v", name, err)
		}
		_, err = db.Query(ctx, "CREATE (:Person {name: 'x', n: 9223372036854775807 + 1})", nil)
		if !errors.Is(err, intreccio.ErrIntegerOverflow) {
			t.Errorf("%s: overflowing write = %v, want ErrIntegerOverflow", name, err)
		}
	}
	// Unique constraints replicate, and a violation keeps its type.
	if _, err := lead.Query(ctx, "CREATE CONSTRAINT FOR (p:Person) REQUIRE p.name IS UNIQUE", nil); err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]*intreccio.DB{"follower": follower, "client": client} {
		if _, err := db.Query(ctx, "CREATE (:Person {name: '1'})", nil); !errors.Is(err, intreccio.ErrConstraintViolation) {
			t.Errorf("%s: duplicate write = %v, want ErrConstraintViolation", name, err)
		}
	}
	// The dataless client forwards default reads too.
	if _, err := client.Query(ctx, matchNames, nil, intreccio.WithMaxRows(2)); !errors.Is(err, intreccio.ErrResultLimit) {
		t.Errorf("client default read over the limit = %v, want ErrResultLimit", err)
	}
}
