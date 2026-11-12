package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio"
	"github.com/giovibal/intreccio/internal/wire"
)

// dropConns closes every connection the server holds, leaving it listening:
// what its peers see when the network fails mid-request.
func (s *forwardServer) dropConns() {
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func outcomeUnknown(err error) bool {
	var remote *wire.Error
	return errors.As(err, &remote) && remote.Code == wire.CodeOutcomeUnknown
}

// TestWriteWithALostReplyIsNotRetried covers #77 at the transport: a write
// whose reply is lost has run on the other member, so the pool must not send
// it again. A read in the same situation is retried, as before.
func TestWriteWithALostReplyIsNotRetried(t *testing.T) {
	var (
		srv  *forwardServer
		runs atomic.Int32
	)
	addr, srv := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		if runs.Add(1) == 1 {
			srv.dropConns() // executed, and the reply never leaves
		}
		return echo(ctx, req)
	})
	pool := newTestPool(t)

	_, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "CREATE (n)", Write: true})
	if !outcomeUnknown(err) {
		t.Fatalf("write with a lost reply: err = %v, want an outcome-unknown error", err)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("the write ran %d times, want 1", n)
	}

	// The pool recovers: the next write goes through on a fresh connection.
	if _, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "CREATE (m)", Write: true}); err != nil {
		t.Fatalf("write after the failure: %v", err)
	}

	// A read is safe to repeat.
	runs.Store(0)
	cols, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "MATCH (n) RETURN n"})
	if err != nil || len(cols) != 1 {
		t.Fatalf("read with a lost reply: %v, %v, want a transparent retry", cols, err)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("the read ran %d times, want 2", n)
	}
}

// A write that never left is still sent: on a pooled connection already known
// to be dead the pool dials again, and a member that is not there is a plain
// failure, not an unknown outcome.
func TestUnsentWriteIsNotAnUnknownOutcome(t *testing.T) {
	var runs atomic.Int32
	addr, _ := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		runs.Add(1)
		return echo(ctx, req)
	})
	pool := newTestPool(t)
	ctx := context.Background()
	write := wire.Request{Cypher: "CREATE (n)", Write: true}

	if _, _, err := pool.call(ctx, addr, write); err != nil {
		t.Fatalf("first write: %v", err)
	}
	pool.connTo(addr).Close() // the pooled connection is known broken
	if _, _, err := pool.call(ctx, addr, write); err != nil {
		t.Fatalf("write after the pooled connection died: %v", err)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("two writes ran %d times", n)
	}

	_, _, err := pool.call(ctx, "127.0.0.1:1", write)
	if err == nil || outcomeUnknown(err) {
		t.Fatalf("write to an unreachable member: err = %v, want a plain failure", err)
	}

	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := pool.call(done, addr, write); !errors.Is(err, context.Canceled) || outcomeUnknown(err) {
		t.Fatalf("write with a context already done: err = %v, want context.Canceled", err)
	}
}

// When the caller stops waiting for a write that was sent, it cannot know
// whether the write was applied.
func TestAbandonedWriteHasAnUnknownOutcome(t *testing.T) {
	started := make(chan struct{}, 1)
	addr, _ := startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	pool := newTestPool(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, err := pool.call(ctx, addr, wire.Request{Cypher: "CREATE (n)", Write: true})
	if !outcomeUnknown(err) {
		t.Fatalf("abandoned write: err = %v, want an outcome-unknown error", err)
	}

	// A read that is abandoned is just cancelled.
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, _, err = pool.call(ctx, addr, wire.Request{Cypher: "MATCH (n) RETURN n"})
	if !errors.Is(err, context.Canceled) || outcomeUnknown(err) {
		t.Fatalf("abandoned read: err = %v, want context.Canceled", err)
	}
}

// A write whose result cannot be sent back was applied all the same: the
// requester must not take the error for a write that did not happen.
func TestWriteWithAnUnsendableResultHasAnUnknownOutcome(t *testing.T) {
	huge := make([]byte, wire.MaxPayload)
	addr, _ := startServer(t, func(context.Context, wire.Request) ([]string, [][]byte, error) {
		return []string{"x"}, [][]byte{huge}, nil
	})
	pool := newTestPool(t)
	_, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "CREATE (n) RETURN n", Write: true})
	if !outcomeUnknown(err) {
		t.Fatalf("write with an oversized result: err = %v, want an outcome-unknown error", err)
	}
	_, _, err = pool.call(context.Background(), addr, wire.Request{Cypher: "MATCH (n) RETURN n"})
	if err == nil || outcomeUnknown(err) {
		t.Fatalf("read with an oversized result: err = %v, want a plain error", err)
	}
}

// A dataless client moves on to the next voter only when the write provably
// did not happen.
func TestClientDoesNotRepeatAWriteOnAnotherVoter(t *testing.T) {
	type voter struct {
		addr string
		runs atomic.Int32
	}
	newVoter := func(answer func() error) *voter {
		v := &voter{}
		v.addr, _ = startServer(t, func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
			v.runs.Add(1)
			if err := answer(); err != nil {
				return nil, nil, err
			}
			return echo(ctx, req)
		})
		return v
	}
	ok := func() error { return nil }
	cases := []struct {
		name   string
		first  func() error
		second int32 // how many times the second voter must run the write
	}{
		{"outcome unknown", func() error {
			return &wire.Error{Code: wire.CodeOutcomeUnknown, Msg: "cluster: write outcome unknown"}
		}, 0},
		{"a definite answer", func() error {
			return &wire.Error{Code: wire.CodeConflict, Msg: "conflict"}
		}, 0},
		{"unavailable", func() error {
			return &wire.Error{Code: wire.CodeUnavailable, Msg: "shutting down"}
		}, 1},
		{"no leader", func() error { return errors.New("cluster: no leader available") }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newVoter(tc.first), newVoter(ok)
			c := &client{
				peers: []Peer{{ID: "a", ForwardAddr: a.addr}, {ID: "b", ForwardAddr: b.addr}},
				pool:  newTestPool(t),
			}
			c.next.Store(1) // the next request starts from voter a
			_, _, err := c.Forward(context.Background(), wire.Request{Cypher: "CREATE (n)", Write: true})
			if a.runs.Load() != 1 {
				t.Fatalf("the first voter ran the write %d times, want 1", a.runs.Load())
			}
			if got := b.runs.Load(); got != tc.second {
				t.Errorf("the second voter ran the write %d times, want %d (err = %v)", got, tc.second, err)
			}
			if (err == nil) != (tc.second == 1) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// Only an error raised before the entry reached the Raft log proves that a
// write was not replicated.
func TestReplicateErrorClassification(t *testing.T) {
	for _, err := range []error{raft.ErrNotLeader, raft.ErrEnqueueTimeout, raft.ErrLeadershipTransferInProgress} {
		if got := replicateError(err); outcomeUnknown(got) || !errors.Is(got, err) {
			t.Errorf("replicateError(%v) = %v, want a plain failure wrapping it", err, got)
		}
	}
	for _, err := range []error{raft.ErrLeadershipLost, raft.ErrRaftShutdown, errors.New("log store: disk full")} {
		if got := replicateError(err); !outcomeUnknown(got) {
			t.Errorf("replicateError(%v) = %v, want an outcome-unknown error", err, got)
		}
	}
}

// TestLostReplyNeverDuplicatesAWrite covers #77 end to end: the leader applies
// a write and the connection drops before its reply leaves. The write must be
// applied exactly once, and the caller told that its outcome is unknown, on
// each of the three paths a forwarded write can take.
func TestLostReplyNeverDuplicatesAWrite(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	var followers []Peer
	for i, p := range tc.peers {
		if i != leader {
			followers = append(followers, p)
		}
	}

	// The leader runs each :Once write and then loses the connections it was
	// asked on — the first time it sees it, so that a write sent again would
	// go through and show up as a second node.
	real := tc.nodes[leader].localExec.Load()
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	tc.nodes[leader].SetLocalExecutor(func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		cols, rows, err := (*real)(ctx, req)
		if err == nil && strings.Contains(req.Cypher, ":Once {path") && req.Write {
			mu.Lock()
			first := !seen[req.Cypher]
			seen[req.Cypher] = true
			mu.Unlock()
			if first {
				tc.nodes[leader].fwd.dropConns()
			}
		}
		return cols, rows, err
	})

	clientTo := func(id string, peers []Peer) *intreccio.DB {
		db, err := Open(Config{NodeID: id, Role: RoleClient, Peers: peers})
		if err != nil {
			t.Fatalf("open client %s: %v", id, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	paths := []struct {
		name string
		db   *intreccio.DB
	}{
		{"follower-to-leader", tc.dbs[tc.aFollower(t)]},
		{"client-to-leader", clientTo("client-direct", []Peer{tc.peers[leader]})},
		{"client-to-follower-to-leader", clientTo("client-proxied", followers)},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := p.db.Query(ctx, fmt.Sprintf("CREATE (:Once {path: '%s'})", p.name), nil)
			if !errors.Is(err, intreccio.ErrWriteOutcomeUnknown) {
				t.Errorf("err = %v, want ErrWriteOutcomeUnknown", err)
			}
			res, err := tc.dbs[leader].Query(ctx,
				"MATCH (n:Once {path: $p}) RETURN count(n)", map[string]any{"p": p.name}, intreccio.Linearizable())
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got := res.Rows[0][0]; got != int64(1) {
				t.Errorf("the write was applied %v times, want 1", got)
			}

			// The path keeps working afterwards.
			if _, err := p.db.Query(ctx, "CREATE (:After)", nil); err != nil {
				t.Errorf("a later write on the same path: %v", err)
			}
		})
	}
}

// A forwarded write the caller stops waiting for reports both why it stopped
// and that the write may have been applied.
func TestForwardedWriteDeadlineIsAnUnknownOutcome(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)

	// The leader holds the write without answering, as when it is slow to
	// replicate it. (Had it answered that it stopped at the deadline, the
	// write would be known not to have happened.)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	real := tc.nodes[leader].localExec.Load()
	tc.nodes[leader].SetLocalExecutor(func(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
		if !req.Write {
			return (*real)(ctx, req)
		}
		<-release
		return nil, nil, errors.New("released by the test")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := tc.dbs[tc.aFollower(t)].Query(ctx, "CREATE (:Slow)", nil)
	if !errors.Is(err, intreccio.ErrWriteOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want ErrWriteOutcomeUnknown and context.DeadlineExceeded", err)
	}
}

// A leader that reports it stopped at the requester's deadline did not apply
// the write: that stays a plain deadline error.
func TestRemoteDeadlineIsADefiniteAnswer(t *testing.T) {
	addr, _ := startServer(t, func(context.Context, wire.Request) ([]string, [][]byte, error) {
		return nil, nil, &wire.Error{Code: wire.CodeDeadlineExceeded, Msg: "context deadline exceeded"}
	})
	pool := newTestPool(t)
	_, _, err := pool.call(context.Background(), addr, wire.Request{Cypher: "CREATE (n)", Write: true})
	var remote *wire.Error
	if !errors.As(err, &remote) || remote.Code != wire.CodeDeadlineExceeded {
		t.Fatalf("err = %v, want the member's deadline error", err)
	}
}
