// Package cluster spike: validates that the Raft stack (hashicorp/raft +
// raft-boltdb) and Badger snapshot round-trip are pure Go and behave as the
// clustering design assumes, before any real wiring (Phase A of the plan).
package cluster

import (
	"bytes"
	"encoding/binary"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
)

// spikeFSM applies opaque commands by storing them in a local Badger store,
// keyed by the Raft log index. This mirrors the real FSM model: every node
// applies the same replicated effects into its own local Store.
type spikeFSM struct{ store *badgerstore.Store }

func cmdKey(index uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, index)
	return k
}

func (f *spikeFSM) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	return f.store.Update(func(txn storage.Txn) error {
		return txn.Set(cmdKey(l.Index), append([]byte(nil), l.Data...))
	})
}

func (f *spikeFSM) Snapshot() (raft.FSMSnapshot, error) { return noopSnapshot{}, nil }
func (f *spikeFSM) Restore(rc io.ReadCloser) error      { return rc.Close() }

type noopSnapshot struct{}

func (noopSnapshot) Persist(sink raft.SnapshotSink) error { return sink.Close() }
func (noopSnapshot) Release()                             {}

type spikeNode struct {
	raft  *raft.Raft
	fsm   *spikeFSM
	trans *raft.InmemTransport
	id    raft.ServerID
}

func newSpikeNode(t *testing.T, id string) *spikeNode {
	t.Helper()

	store, err := badgerstore.OpenInMemory()
	if err != nil {
		t.Fatalf("badger open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// raft-boltdb log + stable store (file-backed) — validates the pure-Go
	// bbolt log store the design relies on.
	boltPath := filepath.Join(t.TempDir(), "raft.db")
	boltStore, err := raftboltdb.NewBoltStore(boltPath)
	if err != nil {
		t.Fatalf("raft-boltdb open: %v", err)
	}
	t.Cleanup(func() { _ = boltStore.Close() })

	snaps := raft.NewInmemSnapshotStore()
	_, trans := raft.NewInmemTransport(raft.ServerAddress(id))

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(id)
	cfg.LogLevel = "ERROR"
	// Fast timeouts so the spike elects a leader in milliseconds (kept generous
	// enough to stay stable under the race detector).
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.LeaderLeaseTimeout = 100 * time.Millisecond
	cfg.CommitTimeout = 10 * time.Millisecond

	fsm := &spikeFSM{store: store}
	r, err := raft.NewRaft(cfg, fsm, boltStore, boltStore, snaps, trans)
	if err != nil {
		t.Fatalf("raft.NewRaft: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown().Error() })

	return &spikeNode{raft: r, fsm: fsm, trans: trans, id: cfg.LocalID}
}

// TestSpikeRaftReplication brings up a 3-node in-process Raft cluster over the
// inmem transport and asserts a command applied on the leader replicates to all
// three local Badger stores.
func TestSpikeRaftReplication(t *testing.T) {
	ids := []string{"n1", "n2", "n3"}
	nodes := make([]*spikeNode, len(ids))
	for i, id := range ids {
		nodes[i] = newSpikeNode(t, id)
	}

	// Wire the inmem transports together (full mesh).
	for _, a := range nodes {
		for _, b := range nodes {
			if a != b {
				a.trans.Connect(b.trans.LocalAddr(), b.trans)
			}
		}
	}

	// Bootstrap the cluster on a single node with all three voters.
	servers := make([]raft.Server, len(nodes))
	for i, n := range nodes {
		servers[i] = raft.Server{
			ID:      n.id,
			Address: n.trans.LocalAddr(),
		}
	}
	if err := nodes[0].raft.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	leader := waitForLeader(t, nodes, 5*time.Second)

	const n = 20
	for i := range n {
		payload := []byte{byte(i)}
		if err := leader.raft.Apply(payload, time.Second).Error(); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	// Every node's local store must converge to n keys.
	for _, node := range nodes {
		waitForKeyCount(t, node, n, 5*time.Second)
	}
}

// TestSpikeBadgerSnapshotRoundTrip validates Badger Backup/Load, the mechanism
// the real FSM Snapshot/Restore will use. It uses badger.DB directly because
// Backup/Load are engine-level operations not exposed by the storage adapter.
func TestSpikeBadgerSnapshotRoundTrip(t *testing.T) {
	openMem := func() *badger.DB {
		opts := badger.DefaultOptions("").WithInMemory(true)
		opts.Logger = nil
		db, err := badger.Open(opts)
		if err != nil {
			t.Fatalf("badger open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	src := openMem()
	want := map[string]string{"alpha": "1", "beta": "2", "gamma": "3"}
	if err := src.Update(func(txn *badger.Txn) error {
		for k, v := range want {
			if err := txn.Set([]byte(k), []byte(v)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if _, err := src.Backup(&buf, 0); err != nil {
		t.Fatalf("backup: %v", err)
	}

	dst := openMem()
	if err := dst.Load(&buf, 16); err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := dst.View(func(txn *badger.Txn) error {
		for k, v := range want {
			item, err := txn.Get([]byte(k))
			if err != nil {
				return err
			}
			got, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			if string(got) != v {
				t.Errorf("key %q = %q, want %q", k, got, v)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func waitForLeader(t *testing.T, nodes []*spikeNode, timeout time.Duration) *spikeNode {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.raft.State() == raft.Leader {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected within timeout")
	return nil
}

func waitForKeyCount(t *testing.T, node *spikeNode, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int
	for time.Now().Before(deadline) {
		got = countKeys(t, node.fsm.store)
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node %s: %d keys, want %d", node.id, got, want)
}

func countKeys(t *testing.T, store *badgerstore.Store) int {
	t.Helper()
	var count int
	if err := store.View(func(txn storage.Txn) error {
		it := txn.Scan(nil)
		defer func() { _ = it.Close() }()
		for ; it.Valid(); it.Next() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return count
}
