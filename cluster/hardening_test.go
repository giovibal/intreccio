package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giovibal/mycypher"
)

// TestSnapshotRestoreOnRestart forces a Raft snapshot, writes more, then restarts
// the node. Recovery must restore from the snapshot and replay the trailing log,
// exercising fsm.Snapshot (streaming to a temp file) and fsm.Restore.
func TestSnapshotRestoreOnRestart(t *testing.T) {
	dir := t.TempDir()
	raftAddr := freeAddr(t)
	fwdAddr := freeAddr(t)
	cfg := Config{
		NodeID:      "n1",
		DataDir:     dir,
		BindAddr:    raftAddr,
		ForwardAddr: fwdAddr,
		Role:        RoleVoter,
		Bootstrap:   true,
		Peers:       []Peer{{ID: "n1", RaftAddr: raftAddr, ForwardAddr: fwdAddr}},
	}
	ctx := context.Background()

	node, err := newNode(cfg)
	if err != nil {
		t.Fatalf("newNode: %v", err)
	}
	db := mycypher.New(node)

	// Writes that end up inside the snapshot.
	for _, name := range []string{"A", "B"} {
		if _, err := db.Query(ctx, "CREATE (n:Person {name: $n})", map[string]any{"n": name}); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Force a snapshot, then write more (these land in the trailing log).
	if err := node.raft.Snapshot().Error(); err != nil {
		t.Fatalf("force snapshot: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "snapshots")); len(entries) == 0 {
		t.Fatalf("no snapshot was persisted under %s/snapshots", dir)
	}
	for _, name := range []string{"C", "D"} {
		if _, err := db.Query(ctx, "CREATE (n:Person {name: $n})", map[string]any{"n": name}); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: recovery = restore snapshot (A,B) + replay trailing log (C,D).
	cfg.Bootstrap = false
	node2, err := newNode(cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = node2.Close() }()
	db2 := mycypher.New(node2)

	got := mustQuery(t, db2, matchNames)
	if !equal(got, []string{"A", "B", "C", "D"}) {
		t.Fatalf("after snapshot+restart: %v, want [A B C D]", got)
	}
}

// TestQuorumLossHaltsWrites verifies the cluster preserves safety: with a
// majority of voters down there is no quorum, so writes fail rather than risk a
// split brain.
func TestQuorumLossHaltsWrites(t *testing.T) {
	tc := startCluster(t, 3, func(c *Config) { c.ApplyTimeout = 2 * time.Second })
	ctx := context.Background()

	if _, err := tc.dbs[tc.leaderIndex(t)].Query(ctx, "CREATE (n:Person {name: 'A'})", nil); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Kill two of the three voters, leaving one without quorum.
	killed := 0
	survivor := -1
	for i := range tc.nodes {
		if killed < 2 {
			_ = tc.nodes[i].Close()
			tc.nodes[i] = nil
			killed++
		} else {
			survivor = i
		}
	}

	// The lone survivor cannot commit a write: it must fail (no split brain).
	if _, err := tc.dbs[survivor].Query(ctx, "CREATE (n:Person {name: 'Z'})", nil); err == nil {
		t.Fatal("expected write to fail without a quorum, but it succeeded")
	}
}

// TestRejoinCatchUpAfterDowntime verifies that a node which is offline while the
// rest of the cluster keeps committing writes fully re-syncs on restart — here
// via the InstallSnapshot path, because the leader's log is compacted past the
// follower's last index while it is down.
func TestRejoinCatchUpAfterDowntime(t *testing.T) {
	tc := startCluster(t, 3, func(c *Config) {
		// Small retention: a forced snapshot then compacts the log well past a
		// behind follower, forcing a full snapshot transfer on rejoin.
		c.SnapshotThreshold = 8
		c.TrailingLogs = 8
	})
	leader := tc.leaderIndex(t)

	// Initial data, then take a follower offline.
	for i := range 5 {
		createPerson(t, tc.dbs[leader], fmt.Sprintf("p%02d", i))
	}
	down := tc.aFollower(t)
	if err := tc.nodes[down].Close(); err != nil {
		t.Fatalf("close follower: %v", err)
	}
	tc.nodes[down] = nil

	// Lots of writes while it is offline, then compact the leader's log.
	for i := 5; i < 45; i++ {
		createPerson(t, tc.dbs[leader], fmt.Sprintf("p%02d", i))
	}
	if err := tc.nodes[leader].raft.Snapshot().Error(); err != nil {
		t.Fatalf("force snapshot: %v", err)
	}
	// A few writes after the snapshot become trailing log replayed post-install.
	for i := 45; i < 50; i++ {
		createPerson(t, tc.dbs[leader], fmt.Sprintf("p%02d", i))
	}
	const total = 50

	if got := personCount(t, tc.dbs[leader], mycypher.Linearizable()); got != total {
		t.Fatalf("leader count = %d, want %d", got, total)
	}

	// Bring the node back: its LOCAL replica must converge to the full dataset
	// (the leader installs a snapshot, then replays the trailing log).
	tc.restart(t, down)
	waitForCountLocal(t, tc.dbs[down], total, 20*time.Second)

	// The node held no snapshot before it crashed (only 5 writes, below the
	// threshold, and no time-based snapshot fired), so a non-zero last snapshot
	// index proves the catch-up went through InstallSnapshot from the leader.
	if idx := tc.nodes[down].raft.Stats()["last_snapshot_index"]; idx == "" || idx == "0" {
		t.Fatalf("expected an installed snapshot on rejoin, got last_snapshot_index=%q", idx)
	}
}

// restart reopens node i from its existing data directory (Bootstrap: false).
func (tc *testCluster) restart(t *testing.T, i int) {
	t.Helper()
	cfg := tc.configs[i]
	cfg.Bootstrap = false
	node, err := newNode(cfg)
	if err != nil {
		t.Fatalf("restart node %d: %v", i, err)
	}
	tc.nodes[i] = node
	tc.dbs[i] = mycypher.New(node)
}

func createPerson(t *testing.T, db *mycypher.DB, name string) {
	t.Helper()
	if _, err := db.Query(context.Background(), "CREATE (n:Person {name: $n})",
		map[string]any{"n": name}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func personCount(t *testing.T, db *mycypher.DB, opts ...mycypher.QueryOption) int64 {
	t.Helper()
	res, err := db.Query(context.Background(), "MATCH (p:Person) RETURN count(p) AS c", nil, opts...)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("count: unexpected result shape %v", res.Rows)
	}
	return toInt64(t, res.Rows[0][0])
}

func waitForCountLocal(t *testing.T, db *mycypher.DB, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int64
	for time.Now().Before(deadline) {
		// Local (non-linearizable) read on the rejoining node.
		res, err := db.Query(context.Background(), "MATCH (p:Person) RETURN count(p) AS c", nil)
		if err == nil && len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
			got = toInt64(t, res.Rows[0][0])
			if got == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("local replica did not converge: count = %d, want %d", got, want)
}

func toInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		t.Fatalf("count value is not an integer: %T", v)
		return 0
	}
}
