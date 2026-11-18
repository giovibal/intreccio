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
	"github.com/giovibal/intreccio/internal/wire"
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
	waitStaleChecks(t, tc, a)

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
	if !checksOn(t, node) {
		t.Fatal("a single voter should have turned the check on with its first write")
	}
	for name, ext := range map[string][]byte{
		"stale base":                 encodeBase(applied + 100),
		"malformed":                  {extBaseV1, 0x80},
		"unknown version":            {9, 1},
		"no base once checks are on": nil,
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

// checksOn reports whether node's state has the stale-write check turned on.
func checksOn(t *testing.T, node *raftNode) bool {
	t.Helper()
	var on bool
	if err := node.store.View(func(txn storage.Txn) (err error) {
		on, err = catalog.StaleChecks(txn)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return on
}

// waitStaleChecks writes through the leader at index lead until the cluster
// has turned the stale-write check on: the first write after every member
// answered the version probe turns it on.
func waitStaleChecks(t *testing.T, tc *testCluster, lead int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !checksOn(t, tc.nodes[lead]) {
		if time.Now().After(deadline) {
			t.Fatal("the cluster never turned the stale-write check on")
		}
		run(t, tc.dbs[lead], "CREATE (:Warmup)")
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOlderMemberDisablesStaleChecks covers the upgrade gate of #98: while a
// member speaks a protocol version older than the check (a rolling upgrade
// not yet finished), it would apply an entry the upgraded members refuse. So
// the cluster does not turn the check on, and every replica applies the same
// entries: they may lose the update #98 is about, as before the check, but
// they never diverge.
func TestOlderMemberDisablesStaleChecks(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	a := tc.leaderIndex(t)
	b := (a + 1) % 3
	old := (a + 2) % 3
	oldAddr := tc.peers[old].ForwardAddr
	for _, nd := range tc.nodes {
		real := nd.dialVersion
		nd.probeVersion = func(ctx context.Context, addr string) (uint16, error) {
			if addr == oldAddr {
				return wire.VersionStaleWriteCheck - 1, nil
			}
			return real(ctx, addr)
		}
	}
	run(t, tc.dbs[a], "CREATE (:Counter {v: 0})")
	for range 5 { // let the probe answer, and give the check every chance
		time.Sleep(100 * time.Millisecond)
		run(t, tc.dbs[a], "CREATE (:Warmup)")
	}
	if checksOn(t, tc.nodes[a]) {
		t.Fatal("the stale-write check was turned on with a member on an older version")
	}

	// The #98 sequence.
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
				<-release
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
	if n := calls.Load(); n != 1 {
		t.Errorf("the Update function ran %d times, want 1: an entry was checked and refused", n)
	}
	want := counter(t, tc.dbs[a])
	for i := range tc.dbs {
		waitForCounter(t, tc.dbs[i], want) // the replicas agree
	}
}

// TestChecksSurviveUnavailableProbes: once the cluster has turned the check
// on, it stays on whatever the version probes say. A member whose probe
// fails (a timeout, a broken forwarding link) must not let a leader propose
// an unchecked entry: the #98 sequence still ends with both increments.
func TestChecksSurviveUnavailableProbes(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	a := tc.leaderIndex(t)
	b := (a + 1) % 3
	run(t, tc.dbs[a], "CREATE (:Counter {v: 0})")
	waitStaleChecks(t, tc, a)

	// From now on no member answers a probe, and no answer is remembered.
	for _, nd := range tc.nodes {
		nd.peerVersions.Clear()
		nd.probeVersion = func(context.Context, string) (uint16, error) {
			return 0, context.DeadlineExceeded
		}
	}

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
				<-release
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
		t.Errorf("counter = %d, want 2: an unchecked entry overwrote an update", got)
	}
	for i := range tc.dbs {
		waitForCounter(t, tc.dbs[i], 2)
	}
}

// heldIncrement runs the #98 sequence: an increment staged on leader a and
// held while leadership goes a -> b -> a, with an increment committed on b
// meanwhile. It returns the held Update's error and the second increment's.
func heldIncrement(t *testing.T, tc *testCluster, a, b int) (held, onB error) {
	t.Helper()
	ctx := context.Background()
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
				<-release
			}
			return nil
		})
	}()
	<-staged
	tc.transferTo(t, a, b)
	_, onB = tc.dbs[b].Query(ctx, "MATCH (c:Counter) SET c.v = c.v + 1", nil)
	tc.transferTo(t, b, a)
	close(release)
	return <-done, onB
}

// TestUnknownMemberRefusesUnprotectedWrites: before the check is on, a member
// whose version is unknown (its probe times out, though Raft reaches it)
// might be older, and then a checked entry would not be checked there, or
// newer, and then an unchecked one could lose an update. A write is refused
// with a retryable error rather than acknowledged unprotected: no warm-up,
// no lost update.
func TestUnknownMemberRefusesUnprotectedWrites(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	a := tc.leaderIndex(t)
	b := (a + 1) % 3
	silent := tc.peers[(a+2)%3].ForwardAddr
	var probeDown atomic.Bool
	probeDown.Store(true)
	for _, nd := range tc.nodes {
		real := nd.dialVersion
		nd.probeVersion = func(ctx context.Context, addr string) (uint16, error) {
			if addr == silent && probeDown.Load() {
				return 0, context.DeadlineExceeded
			}
			return real(ctx, addr)
		}
	}

	_, err := tc.dbs[a].Query(ctx, "CREATE (:Counter {v: 0})", nil)
	if !errors.Is(err, intreccio.ErrConflict) {
		t.Fatalf("write with a member of unknown version: err = %v, want a retryable ErrConflict", err)
	}
	if got := counterNodes(t, tc.dbs[a]); got != 0 {
		t.Fatalf("Counter nodes = %d, want 0: the refused write was applied", got)
	}

	// The member answers again: the next write is accepted and turns the
	// check on, and the #98 sequence keeps both increments.
	probeDown.Store(false)
	run(t, tc.dbs[a], "CREATE (:Counter {v: 0})")
	if !checksOn(t, tc.nodes[a]) {
		t.Fatal("the first write with every member known did not turn the check on")
	}
	held, onB := heldIncrement(t, tc, a, b)
	if held != nil || onB != nil {
		t.Fatalf("increments: held %v, on b %v", held, onB)
	}
	if got := counter(t, tc.dbs[a]); got != 2 {
		t.Errorf("counter = %d, want 2", got)
	}
}

func counterNodes(t *testing.T, db *intreccio.DB) int64 {
	t.Helper()
	res, err := db.Query(context.Background(), "MATCH (c:Counter) RETURN count(c)", nil, intreccio.Linearizable())
	if err != nil {
		t.Fatal(err)
	}
	return toInt64(t, res.Rows[0][0])
}

// TestConfiguredVoterNotYetAdmitted: a voter listed in Peers that has not
// joined yet will apply the whole log once addVoters admits it, so the check
// cannot be turned on, and no write accepted unprotected, before its version
// is known.
func TestConfiguredVoterNotYetAdmitted(t *testing.T) {
	ctx := context.Background()
	peers := make([]Peer, 3)
	for i := range peers {
		peers[i] = Peer{ID: fmt.Sprintf("n%d", i+1), RaftAddr: freeAddr(t), ForwardAddr: freeAddr(t)}
	}
	start := func(i int) *raftNode {
		node, err := newNode(Config{
			NodeID: peers[i].ID, DataDir: t.TempDir(), BindAddr: peers[i].RaftAddr,
			ForwardAddr: peers[i].ForwardAddr, Role: RoleVoter, Bootstrap: i == 0, Peers: peers,
		})
		if err != nil {
			t.Fatalf("start %s: %v", peers[i].ID, err)
		}
		t.Cleanup(func() { _ = node.Close() })
		return node
	}
	n1, _ := start(0), start(1)
	deadline := time.Now().Add(10 * time.Second)
	for n1.raft.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("n1 never became leader")
		}
		time.Sleep(20 * time.Millisecond)
	}
	db := wrap(n1)

	// n3 is configured but not running: its version is unknown.
	if _, err := db.Query(ctx, "CREATE (:Counter {v: 0})", nil); !errors.Is(err, intreccio.ErrConflict) {
		t.Fatalf("write before n3 is known: err = %v, want a retryable ErrConflict", err)
	}
	if checksOn(t, n1) {
		t.Fatal("the check was turned on before every configured voter was known")
	}

	// Once it runs, writes go through and turn the check on.
	start(2)
	deadline = time.Now().Add(15 * time.Second)
	for {
		_, err := db.Query(ctx, "CREATE (:Counter {v: 0})", nil)
		if err == nil {
			break
		}
		if !errors.Is(err, intreccio.ErrConflict) || time.Now().After(deadline) {
			t.Fatalf("write after n3 started: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !checksOn(t, n1) {
		t.Error("the check is still off with every configured voter known")
	}
}

// TestInPlaceUpgradeTurnsTheCheckOn: the last legacy member is upgraded in
// place, with the same leader throughout. The leader must notice it from a
// new answer, not keep its old one forever, and turn the check on with a
// later write, without a restart or a leadership change.
func TestInPlaceUpgradeTurnsTheCheckOn(t *testing.T) {
	tc := startCluster(t, 3)
	a := tc.leaderIndex(t)
	legacyAddr := tc.peers[(a+2)%3].ForwardAddr
	var upgraded atomic.Bool
	var asked atomic.Int32
	for _, nd := range tc.nodes {
		real := nd.dialVersion
		nd.legacyRefresh = 50 * time.Millisecond
		nd.probeVersion = func(ctx context.Context, addr string) (uint16, error) {
			if addr != legacyAddr {
				return real(ctx, addr)
			}
			asked.Add(1)
			if upgraded.Load() {
				return wire.VersionStaleWriteCheck, nil
			}
			return wire.VersionStaleWriteCheck - 1, nil
		}
	}

	run(t, tc.dbs[a], "CREATE (:Counter {v: 0})")
	if checksOn(t, tc.nodes[a]) {
		t.Fatal("the check is on with a legacy member")
	}

	upgraded.Store(true) // the member restarts on the new build
	deadline := time.Now().Add(10 * time.Second)
	for !checksOn(t, tc.nodes[a]) {
		if time.Now().After(deadline) {
			t.Fatalf("the check never turned on after the upgrade (member asked %d times)", asked.Load())
		}
		time.Sleep(60 * time.Millisecond)
		run(t, tc.dbs[a], "CREATE (:Warmup)")
	}
	if tc.nodes[a].raft.State() != raft.Leader {
		t.Error("leadership changed during the test")
	}
	if n := asked.Load(); n < 2 {
		t.Errorf("the upgraded member was asked %d times, want at least 2", n)
	}
}
