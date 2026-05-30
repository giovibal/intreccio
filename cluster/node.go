// Package cluster provides optional Raft-replicated clustering for mycypher.
// It is linked only when the embedding program imports it (via cluster.Open),
// so the default embedded/single-binary path stays pure Go and
// dependency-light. See docs/adr/0007-clustering-raft.md.
package cluster

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/giovibal/mycypher/internal/storage"
	badgerstore "github.com/giovibal/mycypher/internal/storage/badger"
)

const (
	// applyTimeout bounds a single replicated write.
	applyTimeout = 10 * time.Second
	// snapshotsRetained is how many Raft snapshots to keep on disk.
	snapshotsRetained = 2
	// leaderWaitTimeout bounds startup until a leader exists.
	leaderWaitTimeout = 15 * time.Second
)

// ErrNotImplemented is returned for roles/paths not yet wired (Phase B scope).
var ErrNotImplemented = errors.New("cluster: not implemented in this phase")

// Node is a clustered mycypher node: a Raft replica wrapping a local store.
type Node struct {
	cfg   Config
	raft  *raft.Raft
	store *badgerstore.Store
	fsm   *fsm
	trans *raft.NetworkTransport
	log   *raftboltdb.BoltStore
}

// newNode starts a clustered node. Phase B supports a single RoleVoter that
// bootstraps itself; multi-node membership and the client role arrive in
// Phase C/D.
func newNode(cfg Config) (*Node, error) {
	if cfg.Role != RoleVoter {
		return nil, fmt.Errorf("%w: role %d", ErrNotImplemented, cfg.Role)
	}
	if cfg.NodeID == "" || cfg.DataDir == "" || cfg.BindAddr == "" {
		return nil, errors.New("cluster: NodeID, DataDir and BindAddr are required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("cluster: data dir: %w", err)
	}

	store, err := badgerstore.Open(filepath.Join(cfg.DataDir, "data"))
	if err != nil {
		return nil, fmt.Errorf("cluster: open store: %w", err)
	}

	boltStore, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft-log.db"))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("cluster: open raft log: %w", err)
	}

	snaps, err := raft.NewFileSnapshotStore(cfg.DataDir, snapshotsRetained, io.Discard)
	if err != nil {
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: snapshot store: %w", err)
	}

	addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
	if err != nil {
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: resolve bind addr: %w", err)
	}
	trans, err := raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second, io.Discard)
	if err != nil {
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: transport: %w", err)
	}

	rcfg := raft.DefaultConfig()
	rcfg.LocalID = raft.ServerID(cfg.NodeID)
	rcfg.LogOutput = io.Discard

	f := newFSM(store, store)
	r, err := raft.NewRaft(rcfg, f, boltStore, boltStore, snaps, trans)
	if err != nil {
		_ = trans.Close()
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: raft: %w", err)
	}

	n := &Node{cfg: cfg, raft: r, store: store, fsm: f, trans: trans, log: boltStore}

	if cfg.Bootstrap {
		fut := r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
			ID:      rcfg.LocalID,
			Address: trans.LocalAddr(),
		}}})
		if err := fut.Error(); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			_ = n.Close()
			return nil, fmt.Errorf("cluster: bootstrap: %w", err)
		}
	}

	if err := n.waitForLeader(leaderWaitTimeout); err != nil {
		_ = n.Close()
		return nil, err
	}
	return n, nil
}

// View runs fn in a local read-only transaction (snapshot read; may be slightly
// stale on a follower). Linearizable reads arrive in Phase C.
func (n *Node) View(fn func(storage.Txn) error) error { return n.store.View(fn) }

// ApplyWrite executes a write against a recording transaction, then replicates
// its effects through Raft. The whole stage→propose→apply sequence is serialized
// so each statement observes the fully-applied state of all prior writes. The
// staging transaction is rolled back locally; the FSM is the only writer.
func (n *Node) ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error) {
	if n.raft.State() != raft.Leader {
		// Phase C adds leader forwarding; for a single voter this node is leader.
		return nil, fmt.Errorf("%w: write on non-leader", ErrNotImplemented)
	}

	var (
		result   any
		writeSet []byte
	)
	err := n.store.Update(func(real storage.Txn) error {
		rec := newRecordTxn(real)
		r, err := stage(rec)
		if err != nil {
			return err
		}
		result = r
		if rec.empty() {
			return errStaged
		}
		writeSet = rec.encode()
		return errStaged // roll back; effects are replicated, not committed here
	})
	if err != nil && !errors.Is(err, errStaged) {
		return nil, err
	}
	if writeSet == nil {
		// No mutations (e.g. a write clause that produced no effects): nothing to
		// replicate; the result already reflects execution.
		return result, nil
	}

	if err := n.raft.Apply(writeSet, applyTimeout).Error(); err != nil {
		return nil, fmt.Errorf("cluster: replicate write: %w", err)
	}
	return result, nil
}

// Close shuts down Raft and releases all resources.
func (n *Node) Close() error {
	var firstErr error
	if n.raft != nil {
		if err := n.raft.Shutdown().Error(); err != nil {
			firstErr = err
		}
	}
	if n.trans != nil {
		if err := n.trans.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.log != nil {
		if err := n.log.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.store != nil {
		if err := n.store.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (n *Node) waitForLeader(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.raft.Leader() != "" {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("cluster: timed out waiting for a leader")
}
