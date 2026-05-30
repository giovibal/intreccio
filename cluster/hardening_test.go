package cluster

import (
	"context"
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
