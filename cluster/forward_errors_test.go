package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

// TestForwardCarriesDeadlineAndMaxRows covers #32 on the serving side: the
// executor runs under the caller's remaining deadline and WithMaxRows.
func TestForwardCarriesDeadlineAndMaxRows(t *testing.T) {
	var (
		gotDeadline bool
		gotLeft     time.Duration
		gotMax      int64
	)
	n := &raftNode{localExec: func(ctx context.Context, _ string, _ map[string]any, maxRows int64) ([]string, [][]any, error) {
		var d time.Time
		d, gotDeadline = ctx.Deadline()
		gotLeft = time.Until(d)
		gotMax = maxRows
		return []string{"x"}, [][]any{{int64(1)}}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args, err := newForwardArgs(ctx, "RETURN 1 AS x", nil, false, false, 7)
	if err != nil {
		t.Fatal(err)
	}
	var reply ForwardReply
	if err := (&forwardService{node: n}).Query(args, &reply); err != nil || reply.Err != "" {
		t.Fatalf("Query: %v / %q", err, reply.Err)
	}
	if !gotDeadline || gotLeft <= 0 || gotLeft > 5*time.Second {
		t.Errorf("executor deadline = %v (set %v), want within 5s", gotLeft, gotDeadline)
	}
	if gotMax != 7 {
		t.Errorf("executor maxRows = %d, want 7", gotMax)
	}

	// Without a caller deadline the executor gets none either.
	args, _ = newForwardArgs(context.Background(), "RETURN 1 AS x", nil, false, false, 0)
	if err := (&forwardService{node: n}).Query(args, &reply); err != nil {
		t.Fatal(err)
	}
	if gotDeadline {
		t.Errorf("executor has a deadline, want none")
	}

	// An already expired deadline fails before anything is sent.
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if _, err := newForwardArgs(expired, "RETURN 1", nil, false, false, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired deadline = %v, want DeadlineExceeded", err)
	}
}

func TestForwardErrorRoundTrip(t *testing.T) {
	for _, w := range wireErrors {
		var rep ForwardReply
		rep.setError(fmt.Errorf("query: %w", w.err))
		if rep.Code != w.code {
			t.Errorf("%v: code = %q, want %q", w.err, rep.Code, w.code)
		}
		err := rep.error()
		if !errors.Is(err, w.err) || err.Error() != "query: "+w.err.Error() {
			t.Errorf("%v: round trip = %v", w.err, err)
		}
	}
	var rep ForwardReply
	rep.setError(errors.New("parse error"))
	if rep.Code != "" || rep.error().Error() != "parse error" {
		t.Errorf("plain error round trip = %q / %v", rep.Code, rep.error())
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
	// The dataless client forwards default reads too.
	if _, err := client.Query(ctx, matchNames, nil, intreccio.WithMaxRows(2)); !errors.Is(err, intreccio.ErrResultLimit) {
		t.Errorf("client default read over the limit = %v, want ErrResultLimit", err)
	}
}
