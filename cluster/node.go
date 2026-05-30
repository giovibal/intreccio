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
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/giovibal/mycypher/internal/storage"
	badgerstore "github.com/giovibal/mycypher/internal/storage/badger"
)

const (
	// defaultApplyTimeout bounds a single replicated write or read barrier when
	// Config.ApplyTimeout is zero.
	defaultApplyTimeout = 10 * time.Second
	// snapshotsRetained is how many Raft snapshots to keep on disk.
	snapshotsRetained = 2
	// leaderWaitTimeout bounds startup until a leader exists.
	leaderWaitTimeout = 15 * time.Second
	// addVoterInterval is the retry cadence for adding peers after bootstrap.
	addVoterInterval = 200 * time.Millisecond
)

// ErrNotImplemented is returned for roles/paths not yet wired.
var ErrNotImplemented = errors.New("cluster: not implemented in this phase")

// errNotLeader indicates a write reached a node that is not the leader.
var errNotLeader = errors.New("cluster: not leader")

// Node is a clustered mycypher node: a Raft replica wrapping a local store.
type Node struct {
	cfg   Config
	raft  *raft.Raft
	store *badgerstore.Store
	fsm   *fsm
	trans *raft.NetworkTransport
	log   *raftboltdb.BoltStore

	// forwardAddrByID maps a Raft ServerID to that node's forwarding RPC address.
	forwardAddrByID map[string]string
	fwdListener     net.Listener
	localExec       localExecutor
	applyTimeout    time.Duration

	// writeMu serializes the stage→propose→apply sequence so each statement sees
	// the fully-applied state of all prior writes.
	writeMu sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
}

// newNode starts a clustered node. Phases B–C support RoleVoter. A single node
// bootstraps and adds the other voters from Peers; the rest start and are added.
func newNode(cfg Config) (*Node, error) {
	if cfg.Role != RoleVoter {
		return nil, fmt.Errorf("%w: role %d", ErrNotImplemented, cfg.Role)
	}
	if cfg.NodeID == "" || cfg.DataDir == "" || cfg.BindAddr == "" || cfg.ForwardAddr == "" {
		return nil, errors.New("cluster: NodeID, DataDir, BindAddr and ForwardAddr are required")
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
	logOutput := cfg.LogOutput
	if logOutput == nil {
		logOutput = io.Discard
	}
	trans, err := raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second, logOutput)
	if err != nil {
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: transport: %w", err)
	}

	rcfg := raft.DefaultConfig()
	rcfg.LocalID = raft.ServerID(cfg.NodeID)
	rcfg.LogOutput = logOutput
	if cfg.SnapshotThreshold > 0 {
		rcfg.SnapshotThreshold = cfg.SnapshotThreshold
	}
	if cfg.TrailingLogs > 0 {
		rcfg.TrailingLogs = cfg.TrailingLogs
	}
	if cfg.SnapshotInterval > 0 {
		rcfg.SnapshotInterval = cfg.SnapshotInterval
	}

	f := newFSM(store, store, cfg.DataDir)
	r, err := raft.NewRaft(rcfg, f, boltStore, boltStore, snaps, trans)
	if err != nil {
		_ = trans.Close()
		_ = boltStore.Close()
		_ = store.Close()
		return nil, fmt.Errorf("cluster: raft: %w", err)
	}

	applyTimeout := cfg.ApplyTimeout
	if applyTimeout <= 0 {
		applyTimeout = defaultApplyTimeout
	}

	n := &Node{
		cfg:             cfg,
		raft:            r,
		store:           store,
		fsm:             f,
		trans:           trans,
		log:             boltStore,
		forwardAddrByID: forwardAddrMap(cfg),
		applyTimeout:    applyTimeout,
		stop:            make(chan struct{}),
	}

	if err := n.startForwardServer(); err != nil {
		_ = n.Close()
		return nil, err
	}

	if cfg.Bootstrap {
		bootErr := r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
			ID:      rcfg.LocalID,
			Address: trans.LocalAddr(),
		}}}).Error()
		switch {
		case bootErr == nil:
			go n.addVoters() // fresh cluster: add the other voters
		case errors.Is(bootErr, raft.ErrCantBootstrap):
			// Already bootstrapped (restart): configuration recovered from the log.
		default:
			_ = n.Close()
			return nil, fmt.Errorf("cluster: bootstrap: %w", bootErr)
		}
	}

	if err := n.waitForLeader(leaderWaitTimeout); err != nil {
		_ = n.Close()
		return nil, err
	}
	return n, nil
}

// forwardAddrMap builds the ServerID→forwarding-address map from the configured
// peer set, always including this node.
func forwardAddrMap(cfg Config) map[string]string {
	m := make(map[string]string, len(cfg.Peers)+1)
	m[cfg.NodeID] = cfg.ForwardAddr
	for _, p := range cfg.Peers {
		m[p.ID] = p.ForwardAddr
	}
	return m
}

// IsLeader reports whether this node is the current Raft leader.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// ReadBarrier blocks until this leader has applied everything committed as of
// the call, providing linearizable reads when followed by a local read.
func (n *Node) ReadBarrier() error {
	if err := n.raft.Barrier(n.applyTimeout).Error(); err != nil {
		return fmt.Errorf("cluster: read barrier: %w", err)
	}
	return nil
}

// Forward sends a query to the leader for execution (used by followers for
// writes and linearizable reads).
func (n *Node) Forward(cypher string, params map[string]any, write, linearizable bool) ([]string, [][]any, error) {
	return n.forward(cypher, params, write, linearizable)
}

// Local reports that a voter can serve reads from its local store.
func (n *Node) Local() bool { return true }

// SetLocalExecutor wires the root package's local query execution, used by the
// leader to run forwarded queries.
func (n *Node) SetLocalExecutor(fn func(cypher string, params map[string]any) ([]string, [][]any, error)) {
	n.localExec = fn
}

// View runs fn in a local read-only transaction (snapshot read; may be slightly
// stale on a follower unless a ReadBarrier preceded it).
func (n *Node) View(fn func(storage.Txn) error) error { return n.store.View(fn) }

// ApplyWrite executes a write against a recording transaction on the leader,
// then replicates its effects through Raft. The staging transaction is rolled
// back locally; the FSM is the only writer. Callers route non-leader writes
// through Forward instead.
func (n *Node) ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error) {
	n.writeMu.Lock()
	defer n.writeMu.Unlock()

	if n.raft.State() != raft.Leader {
		return nil, errNotLeader
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
		return result, nil // no mutations to replicate
	}

	if err := n.raft.Apply(writeSet, n.applyTimeout).Error(); err != nil {
		return nil, fmt.Errorf("cluster: replicate write: %w", err)
	}
	return result, nil
}

// Close shuts down Raft and releases all resources.
func (n *Node) Close() error {
	if n.stop != nil {
		n.stopOnce.Do(func() { close(n.stop) })
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.fwdListener != nil {
		keep(n.fwdListener.Close())
	}
	if n.raft != nil {
		keep(n.raft.Shutdown().Error())
	}
	if n.trans != nil {
		keep(n.trans.Close())
	}
	if n.log != nil {
		keep(n.log.Close())
	}
	if n.store != nil {
		keep(n.store.Close())
	}
	return firstErr
}

// addVoters adds the configured peers to the cluster, retrying until every peer
// is a member or the node shuts down. Only the leader can add voters.
func (n *Node) addVoters() {
	for {
		select {
		case <-n.stop:
			return
		default:
		}

		missing := false
		cfgFut := n.raft.GetConfiguration()
		if err := cfgFut.Error(); err == nil {
			present := make(map[string]bool)
			for _, s := range cfgFut.Configuration().Servers {
				present[string(s.ID)] = true
			}
			for _, p := range n.cfg.Peers {
				if p.ID == n.cfg.NodeID || present[p.ID] {
					continue
				}
				missing = true
				_ = n.raft.AddVoter(raft.ServerID(p.ID), raft.ServerAddress(p.RaftAddr), 0, n.applyTimeout).Error()
			}
		}
		if !missing {
			return
		}
		select {
		case <-n.stop:
			return
		case <-time.After(addVoterInterval):
		}
	}
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
