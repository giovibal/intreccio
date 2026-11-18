package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio"
	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/storage"
)

// transferTo moves leadership from node from to node to and waits for it.
func (tc *testCluster) transferTo(t *testing.T, from, to int) {
	t.Helper()
	p := tc.peers[to]
	if err := tc.nodes[from].raft.LeadershipTransferToServer(raft.ServerID(p.ID), raft.ServerAddress(p.RaftAddr)).Error(); err != nil {
		t.Fatalf("transfer leadership %d -> %d: %v", from, to, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tc.nodes[to].raft.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatalf("node %d did not become leader", to)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func counter(t *testing.T, db *intreccio.DB) int64 {
	t.Helper()
	res, err := db.Query(context.Background(), "MATCH (c:Counter) RETURN c.v", nil, intreccio.Linearizable())
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return toInt64(t, res.Rows[0][0])
}

// TestStagedWriteSurvivesTwoLeadershipChanges covers #98: leader A stages an
// increment and is held before proposing it; leadership moves to B, which
// commits another increment, and back to A. A's write-set was computed on the
// state before B's increment: applied as it is, it would overwrite it and
// lose an acknowledged update. It must be refused and staged again.
func TestStagedWriteSurvivesTwoLeadershipChanges(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	a := tc.leaderIndex(t)
	b := (a + 1) % 3
	run(t, tc.dbs[a], "CREATE (:Counter {v: 0})")

	staged := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- tc.dbs[a].Update(ctx, func(tx *intreccio.Tx) error {
			if _, err := tx.Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil); err != nil {
				return err
			}
			if calls.Add(1) == 1 {
				close(staged)
				<-release // held between staging and proposing
			}
			return nil
		})
	}()
	<-staged

	tc.transferTo(t, a, b)
	run(t, tc.dbs[b], "MATCH (c:Counter) SET c.v = c.v + 1")
	tc.transferTo(t, b, a)
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("the held Update: %v", err)
	}
	if got := counter(t, tc.dbs[a]); got != 2 {
		t.Errorf("counter = %d, want 2: an acknowledged increment was lost", got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the Update function ran %d times, want 2 (staged again once)", n)
	}
	// Every replica holds the same state.
	for i := range tc.dbs {
		waitForCounter(t, tc.dbs[i], 2)
	}
}

func waitForCounter(t *testing.T, db *intreccio.DB, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		res, err := db.Query(context.Background(), "MATCH (c:Counter) RETURN c.v", nil)
		if err == nil && len(res.Rows) == 1 {
			if got = toInt64(t, res.Rows[0][0]); got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("replica counter = %d, want %d", got, want)
}

func TestEntryBaseExtension(t *testing.T) {
	for _, base := range []uint64{0, 1, 300, 1 << 40} {
		got, ok, err := decodeBase(encodeBase(base))
		if err != nil || !ok || got != base {
			t.Errorf("round trip %d = %d, %v, %v", base, got, ok, err)
		}
	}
	if _, ok, err := decodeBase(nil); ok || err != nil {
		t.Errorf("no extension: ok = %v, err = %v; want no check and no error", ok, err)
	}
	for _, bad := range [][]byte{{2, 0}, {extBaseV1}, {extBaseV1, 0x80}, {extBaseV1, 1, 2}} {
		if _, _, err := decodeBase(bad); err == nil {
			t.Errorf("decodeBase(%v): want an error", bad)
		}
	}
}

// TestApplyResponse covers how a leader reads the state machine's answer to
// its own entry, #97 and #98 together: a stale entry is staged again, an
// entry refused everywhere fails plainly, and a local storage failure is an
// unknown outcome, never a success or a retry.
func TestApplyResponse(t *testing.T) {
	cases := []struct {
		name     string
		resp     any
		rejected bool
		check    func(error) bool
	}{
		{"applied", nil, false, func(err error) bool { return err == nil }},
		{"stale: stage again", errStaleWrite, true, func(err error) bool { return err == nil }},
		{"refused everywhere", fmt.Errorf("%w: bad extension", errEntryRefused), false,
			func(err error) bool { return errors.Is(err, errEntryRefused) && !outcomeUnknown(err) }},
		{"replica failed", fmt.Errorf("%w (entry 7: disk)", ErrReplicaFailed), false, outcomeUnknown},
		{"any other error", errors.New("unexpected"), false, outcomeUnknown},
	}
	for _, tc := range cases {
		rejected, err := applyResponse(tc.resp)
		if rejected != tc.rejected || !tc.check(err) {
			t.Errorf("%s: rejected = %v, err = %v", tc.name, rejected, err)
		}
	}
}

// TestRefusalsDoNotFailTheReplica: an entry the state machine refuses for a
// reason every replica shares (stale, or a malformed extension) writes
// nothing and must not put the replica out of service, as a storage failure
// does.
func TestRefusalsDoNotFailTheReplica(t *testing.T) {
	tc := startCluster(t, 1)
	node := tc.nodes[tc.leaderIndex(t)]
	run(t, tc.dbs[0], "CREATE (:Counter {v: 0})")

	var applied uint64
	if err := node.store.View(func(txn storage.Txn) (err error) {
		applied, err = catalog.AppliedIndex(txn)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, ext := range map[string][]byte{
		"stale base":      encodeBase(applied + 100),
		"malformed":       {extBaseV1, 0x80},
		"unknown version": {9, 1},
	} {
		resp := node.fsm.Apply(&raft.Log{Type: raft.LogCommand, Index: applied + 1000, Data: []byte{0}, Extensions: ext})
		if err, _ := resp.(error); err == nil {
			t.Errorf("%s: applied, want a refusal", name)
		}
		if err := node.fsm.failed(); err != nil {
			t.Fatalf("%s: the replica failed: %v", name, err)
		}
	}
	// Still serving, and nothing was written.
	if got := counter(t, tc.dbs[0]); got != 0 {
		t.Errorf("counter = %d, want 0", got)
	}
	run(t, tc.dbs[0], "MATCH (c:Counter) SET c.v = c.v + 1")
	if got := counter(t, tc.dbs[0]); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}
