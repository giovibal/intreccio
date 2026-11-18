// Package cluster provides optional Raft-replicated clustering for intreccio.
// It is linked only when the embedding program imports it (via cluster.Open),
// so the default embedded/single-binary path stays pure Go and
// dependency-light. See docs/adr/0007-clustering-raft.md.
package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/storage"
	badgerstore "github.com/giovibal/intreccio/internal/storage/badger"
	"github.com/giovibal/intreccio/internal/wire"
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

// raftNode is a clustered intreccio node: a Raft replica wrapping a local store.
type raftNode struct {
	cfg   Config
	raft  *raft.Raft
	store *badgerstore.Store
	fsm   *fsm
	trans *raft.NetworkTransport
	log   *raftboltdb.BoltStore

	// forwardAddrByID maps a Raft ServerID to that node's forwarding address.
	forwardAddrByID map[string]string
	// fwd serves the requests other members forward to this node.
	fwd *forwardServer
	// localExec is set once the owning DB exists, possibly while the forward
	// server is already accepting, hence the atomic pointer.
	localExec    atomic.Pointer[localExecutor]
	localSub     atomic.Pointer[localSubscriber]
	applyTimeout time.Duration
	// tlsServer/tlsClient are nil on a plaintext cluster.
	tlsServer *tls.Config
	tlsClient *tls.Config
	// pool reuses outbound forwarding connections (to the leader when proxying).
	pool *connPool

	// writeMu serializes the stage→propose→apply sequence so each statement sees
	// the fully-applied state of all prior writes.
	writeMu sync.Mutex
	// barrierTerm is the Raft term in which this node, as leader, last
	// confirmed that its state machine applied every committed entry. Guarded
	// by writeMu.
	barrierTerm uint64

	// storeMu guards local store access against a snapshot Restore (Badger
	// DropAll+Load). Reads/staging take it shared; the FSM's Restore takes it
	// exclusively. Shared with the fsm.
	storeMu *sync.RWMutex

	stop     chan struct{}
	stopOnce sync.Once

	// shedding is set once shedLeadership has been started (on the first
	// failure); it is never cleared.
	shedding atomic.Bool

	// peerVersions holds, by node ID, the forwarding-protocol version each
	// other member answered with, as a peerVersion (see membersCapability).
	peerVersions sync.Map
	// legacyRefresh is how long an answer older than the stale-write check
	// is trusted before the member is asked again (it may have been
	// upgraded in place). Tests shorten it.
	legacyRefresh time.Duration
	// probeVersion asks the member at a forwarding address which protocol
	// version it speaks. Tests replace it; it is dialVersion otherwise.
	probeVersion func(ctx context.Context, addr string) (uint16, error)
}

// newNode starts a clustered node. Phases B–C support RoleVoter. A single node
// bootstraps and adds the other voters from Peers; the rest start and are added.
func newNode(cfg Config) (*raftNode, error) {
	if cfg.Role != RoleVoter {
		return nil, fmt.Errorf("%w: role %d", ErrNotImplemented, cfg.Role)
	}
	if cfg.NodeID == "" || cfg.DataDir == "" || cfg.BindAddr == "" || cfg.ForwardAddr == "" {
		return nil, errors.New("cluster: NodeID, DataDir, BindAddr and ForwardAddr are required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("cluster: data dir: %w", err)
	}

	// Build the TLS configurations first: it is a configuration error we can
	// report before touching any on-disk state.
	var tlsServer, tlsClient *tls.Config
	if cfg.TLS != nil {
		var err error
		tlsServer, tlsClient, err = cfg.TLS.configs()
		if err != nil {
			return nil, err
		}
	}

	store, err := badgerstore.Open(filepath.Join(cfg.DataDir, "data"))
	if err != nil {
		return nil, fmt.Errorf("cluster: open store: %w", err)
	}
	if err := store.Update(func(txn storage.Txn) error { return catalog.EnsureFormat(txn) }); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("cluster: format: %w", err)
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
	trans, err := newTransport(cfg.BindAddr, addr, tlsServer, tlsClient, logOutput)
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

	storeMu := new(sync.RWMutex)
	f := newFSM(store, store, cfg.DataDir, storeMu)
	// The FSM can fail while Raft replays the log, before the node exists.
	var self atomic.Pointer[raftNode]
	f.onFail = func(error) {
		if n := self.Load(); n != nil {
			n.replicaFailed()
		}
	}
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

	n := &raftNode{
		cfg:             cfg,
		raft:            r,
		store:           store,
		fsm:             f,
		trans:           trans,
		log:             boltStore,
		forwardAddrByID: forwardAddrMap(cfg),
		applyTimeout:    applyTimeout,
		storeMu:         storeMu,
		tlsServer:       tlsServer,
		tlsClient:       tlsClient,
		pool:            newConnPool(tlsClient),
		stop:            make(chan struct{}),
	}
	n.probeVersion = n.dialVersion
	n.legacyRefresh = defaultLegacyRefresh
	self.Store(n)
	if f.failed() != nil {
		n.replicaFailed() // failed during the replay above
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
func (n *raftNode) IsLeader() bool { return n.raft.State() == raft.Leader }

// ReadBarrier blocks until this leader has applied everything committed as of
// the call, providing linearizable reads when followed by a local read.
func (n *raftNode) ReadBarrier() error {
	if err := n.raft.Barrier(n.applyTimeout).Error(); err != nil {
		return fmt.Errorf("cluster: read barrier: %w", err)
	}
	return nil
}

// Forward sends a request to the leader for execution (used by followers for
// writes and linearizable reads). ctx bounds the wait for a reply, and ending
// it stops the execution on the leader.
func (n *raftNode) Forward(ctx context.Context, req wire.Request) ([]string, [][]byte, error) {
	return n.callLeader(ctx, req)
}

// Local reports that a voter can serve reads from its local store.
func (n *raftNode) Local() bool { return true }

// SetLocalExecutor wires the root package's local query execution, used to run
// the requests other members forward to this node.
func (n *raftNode) SetLocalExecutor(fn func(ctx context.Context, req wire.Request) ([]string, [][]byte, error)) {
	exec := localExecutor(fn)
	n.localExec.Store(&exec)
}

// SetLocalSubscriber wires the root package's live-query server, used to run
// the live queries dataless clients subscribe to on this node.
func (n *raftNode) SetLocalSubscriber(fn func(ctx context.Context, req wire.SubscribeRequest, sink wire.BatchSink) error) {
	sub := localSubscriber(fn)
	n.localSub.Store(&sub)
}

// SetChangeHub wires the hub of the owning DB into the FSM, which reports every
// applied write and every snapshot restore to it. This is what lets a voter
// serve live queries.
func (n *raftNode) SetChangeHub(h *changefeed.Hub) { n.fsm.hub.Store(h) }

// View runs fn in a local read-only transaction (snapshot read; may be slightly
// stale on a follower unless a ReadBarrier preceded it). The shared read-lock
// keeps it from overlapping a snapshot Restore.
func (n *raftNode) View(fn func(storage.Txn) error) error {
	if err := n.fsm.failed(); err != nil {
		return err
	}
	n.storeMu.RLock()
	defer n.storeMu.RUnlock()
	return n.store.View(fn)
}

// ApplyWrite executes a write against a recording transaction on the leader,
// then replicates its effects through Raft. The staging transaction is rolled
// back locally; the FSM is the only writer. Callers route non-leader writes
// through Forward instead.
func (n *raftNode) ApplyWrite(stage func(txn storage.Txn) (any, error)) (any, error) {
	n.writeMu.Lock()
	defer n.writeMu.Unlock()

	for attempt := 0; ; attempt++ {
		result, rejected, err := n.applyOnce(stage)
		if !rejected {
			return result, err
		}
		// The write was staged on a state that changed before the entry was
		// applied: every replica refused it, so nothing was written. Stage
		// it again on the current state.
		if attempt+1 >= maxStaleRetries {
			return nil, fmt.Errorf("cluster: write kept being staged on a stale state: %w", storage.ErrConflict)
		}
	}
}

// capability says what the other members of the cluster are known to do
// with the base of an entry (see membersCapability).
type capability int

const (
	// allCheck: every other member checks the base. The cluster may turn
	// the check on.
	allCheck capability = iota
	// someLegacy: every member's version is known, and one is older than
	// the check (a rolling upgrade). Entries go unchecked, so that every
	// replica applies the same ones.
	someLegacy
	// someUnknown: a member has not answered (it is down, unreachable, or
	// not yet admitted). An unchecked entry is safe only if that member is
	// older, a checked one only if it is newer: no write can be protected,
	// and none is accepted until the check is on.
	someUnknown
)

// capabilityProbeTimeout bounds how long a write waits for the members whose
// version it does not know yet.
const capabilityProbeTimeout = 500 * time.Millisecond

// membersCapability finds out, by the protocol version each member answers
// the forwarding handshake with, whether the other members check the base of
// an entry. The members are those of the Raft configuration and those
// configured in Peers: a voter that addVoters has not admitted yet will
// apply the log all the same. Members whose version is not known are asked
// now, in parallel; an answer, legacy ones included, is remembered.
func (n *raftNode) membersCapability() (capability, string) {
	ids := map[string]bool{}
	for _, p := range n.cfg.Peers {
		ids[p.ID] = true
	}
	if future := n.raft.GetConfiguration(); future.Error() == nil {
		for _, srv := range future.Configuration().Servers {
			ids[string(srv.ID)] = true
		}
	}
	delete(ids, n.cfg.NodeID)

	// Ask the members never heard from, and the ones last heard from in an
	// older version once that answer is legacyRefresh old: they may have
	// been upgraded in place. A legacy answer stays until a new one comes,
	// so that a member restarting for its upgrade does not become unknown.
	var ask []string
	now := time.Now()
	for id := range ids {
		pv, ok := n.peerVersions.Load(id)
		if !ok {
			ask = append(ask, id)
			continue
		}
		if p := pv.(peerVersion); p.version < wire.VersionStaleWriteCheck && now.Sub(p.asked) >= n.legacyRefresh {
			n.peerVersions.Store(id, peerVersion{version: p.version, asked: now})
			ask = append(ask, id)
		}
	}
	if len(ask) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), capabilityProbeTimeout)
		var wg sync.WaitGroup
		for _, id := range ask {
			addr, ok := n.forwardAddrByID[id]
			if !ok {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if v, err := n.probeVersion(ctx, addr); err == nil {
					n.peerVersions.Store(id, peerVersion{version: v, asked: time.Now()})
				}
			}()
		}
		wg.Wait()
		cancel()
	}

	result, culprit := allCheck, ""
	for id := range ids {
		pv, ok := n.peerVersions.Load(id)
		switch {
		case !ok:
			return someUnknown, id
		case pv.(peerVersion).version < wire.VersionStaleWriteCheck:
			result, culprit = someLegacy, id
		}
	}
	return result, culprit
}

// peerVersion is a member's answer to the version probe, and when it was
// last asked.
type peerVersion struct {
	version uint16
	asked   time.Time
}

// defaultLegacyRefresh is how long a legacy answer is trusted (see
// raftNode.legacyRefresh): a member upgraded in place is noticed within it,
// and the check turns on with the first write after that.
const defaultLegacyRefresh = 5 * time.Second

// dialVersion connects to a member's forwarding address and returns the
// protocol version it answers the handshake with.
func (n *raftNode) dialVersion(ctx context.Context, addr string) (uint16, error) {
	c, err := dialMux(ctx, addr, n.tlsClient, defaultKeepalive)
	var mismatch *wire.VersionError
	switch {
	case err == nil:
		c.Close()
		return c.version, nil
	case errors.As(err, &mismatch):
		return mismatch.Remote, nil
	default:
		return 0, err
	}
}

// maxStaleRetries bounds how many times ApplyWrite stages a write again after
// the state machine refused it as stale (see applyOnce). A refusal needs a
// leadership change between staging and applying, so this is rarely more
// than one.
const maxStaleRetries = 10

// applyOnce stages a write and proposes its write-set. rejected reports that
// the state machine refused the entry because the state it was staged on had
// changed meanwhile; nothing was applied anywhere, and the caller may stage
// it again. Called with writeMu held.
func (n *raftNode) applyOnce(stage func(txn storage.Txn) (any, error)) (result any, rejected bool, err error) {
	if n.raft.State() != raft.Leader {
		return nil, false, errNotLeader
	}
	if err := n.fsm.failed(); err != nil {
		return nil, false, err // staging would read a state missing a write
	}
	// A newly elected leader can hold committed entries its state machine has
	// not applied yet. Staging against that store would read stale data and
	// replicate a write-set that overwrites newer values, so the first write of
	// each term waits for the state machine to catch up.
	if term := n.raft.CurrentTerm(); term != n.barrierTerm {
		if err := n.raft.Barrier(n.applyTimeout).Error(); err != nil {
			return nil, false, fmt.Errorf("cluster: leader barrier: %w", err)
		}
		n.barrierTerm = term
	}

	// Until the cluster checks entries, find out whether it can start to,
	// and whether an unchecked entry is safe meanwhile.
	capable, member := allCheck, ""
	if on, err := n.staleChecksOn(); err != nil {
		return nil, false, err
	} else if !on {
		capable, member = n.membersCapability()
	}

	result, writeSet, base, checked, err := n.stage(stage, capable == allCheck)
	if err != nil {
		return nil, false, err
	}
	if writeSet == nil {
		return result, false, nil // no mutations to replicate
	}
	if !checked && capable == someUnknown {
		// An unchecked entry could overwrite a newer state, a checked one
		// could be applied unchecked by an older member: refuse the write
		// before anything is proposed, so that it can be retried.
		return nil, false, fmt.Errorf("cluster: cannot protect the write while the version of member %s is unknown: %w", member, storage.ErrConflict)
	}

	var ext []byte
	if checked {
		ext = encodeBase(base)
	}
	future := n.raft.ApplyLog(raft.Log{Data: writeSet, Extensions: ext}, n.applyTimeout)
	if err := future.Error(); err != nil {
		return nil, false, replicateError(err)
	}
	if rejected, err := applyResponse(future.Response()); rejected || err != nil {
		return nil, rejected, err
	}
	return result, false, nil
}

// staleChecksOn reports whether the cluster has turned the stale-write check
// on (catalog.StaleChecks), in this member's state.
func (n *raftNode) staleChecksOn() (bool, error) {
	var on bool
	err := n.View(func(txn storage.Txn) (err error) {
		on, err = catalog.StaleChecks(txn)
		return err
	})
	return on, err
}

// stage runs a write against a recording transaction and returns its result,
// the write-set it produced (nil when it changed nothing), base, the applied
// index of the state it was staged on, and checked, whether the entry must
// carry that base (see encodeBase). The transaction is always rolled back:
// the FSM is the only writer.
//
// Entries are checked once the cluster has turned the check on
// (catalog.StaleChecks). enable turns it on, in the write-set of this very
// write: the caller passes it once every member has said it checks entries.
//
// The store's read lock is released however stage ends, a panic in the
// caller's code included: a Restore takes the lock exclusively and would
// otherwise wait forever, with every later reader queued behind it.
func (n *raftNode) stage(stage func(txn storage.Txn) (any, error), enable bool) (result any, writeSet []byte, base uint64, checked bool, err error) {
	n.storeMu.RLock()
	defer n.storeMu.RUnlock()
	err = n.store.Update(func(real storage.Txn) error {
		// The last command applied to the state staged on. Leadership can
		// change and come back while the caller's code runs, with another
		// leader's writes applied meanwhile: the write-set below would then
		// overwrite them. The state machine refuses it unless the state is
		// still this one.
		var err error
		if base, err = catalog.AppliedIndex(real); err != nil {
			return err
		}
		if checked, err = catalog.StaleChecks(real); err != nil {
			return err
		}
		rec := newRecordTxn(real)
		r, err := stage(rec)
		if err != nil {
			return err
		}
		result = r
		if rec.empty() {
			return errStaged
		}
		if !checked && enable {
			if err := catalog.EnableStaleChecks(rec); err != nil {
				return err
			}
			checked = true
		}
		writeSet = rec.encode()
		return errStaged // roll back; effects are replicated, not committed here
	})
	if err != nil && !errors.Is(err, errStaged) {
		return nil, nil, 0, false, err
	}
	return result, writeSet, base, checked, nil
}

// replicaFailed reacts to the state machine's failure (ErrReplicaFailed):
// live queries are woken so that they end instead of serving a state that
// misses a write, and a leader hands leadership to a healthy voter, since it
// can no longer stage writes.
func (n *raftNode) replicaFailed() {
	if hub := n.fsm.hub.Load(); hub != nil {
		hub.Invalidate()
	}
	if n.shedding.CompareAndSwap(false, true) {
		go n.shedLeadership()
	}
}

// shedLeadershipEvery is how often a replica that has failed once checks
// whether it is failed and holds leadership.
const shedLeadershipEvery = 100 * time.Millisecond

// shedLeadership keeps a failed replica from holding leadership. Its log is
// up to date, so it stays eligible: it can win an election, or be handed
// leadership, after it failed. Each time it finds itself failed and leader it
// transfers leadership to a healthy voter, retrying when a transfer fails.
//
// Started by the first failure, it runs until the node closes, also while a
// restore has cleared the failure: a monitor that exited then could miss a
// second failure arriving as it released its ownership.
func (n *raftNode) shedLeadership() {
	t := time.NewTicker(shedLeadershipEvery)
	defer t.Stop()
	for {
		if n.fsm.failed() != nil && n.raft.State() == raft.Leader {
			_ = n.raft.LeadershipTransfer().Error()
		}
		select {
		case <-n.stop:
			return
		case <-t.C:
		}
	}
}

// replicateError turns the failure of a Raft apply into the error of the
// write. Raft rejects an entry before it enters the log in three cases, and
// only then is the write known not to have happened. After that point the
// entry may still be committed by this leader or by its successor, whatever
// the error says, so the outcome is unknown.
func replicateError(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader),
		errors.Is(err, raft.ErrEnqueueTimeout),
		errors.Is(err, raft.ErrLeadershipTransferInProgress):
		return fmt.Errorf("cluster: replicate write: %w", err)
	}
	return unknownOutcome("replicate write: %v", err)
}

// unknownOutcome is the error of a write that may or may not have been
// applied (ADR 0013). It travels between members with its code, and the root
// package turns it into ErrWriteOutcomeUnknown.
func unknownOutcome(format string, args ...any) error {
	return &wire.Error{Code: wire.CodeOutcomeUnknown, Msg: "cluster: write outcome unknown: " + fmt.Sprintf(format, args...)}
}

// Close shuts down Raft and releases all resources.
func (n *raftNode) Close() error {
	if n.stop != nil {
		n.stopOnce.Do(func() { close(n.stop) })
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if n.fwd != nil {
		keep(n.fwd.shutdown()) // stop accepting; cancel forwarded requests
	}
	if n.pool != nil {
		keep(n.pool.Close())
	}
	if n.raft != nil {
		keep(n.raft.Shutdown().Error())
	}
	if n.fwd != nil {
		// Raft is down, so no forwarded request can still be waiting on it:
		// let them all return before the store goes away under them.
		n.fwd.wait()
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
func (n *raftNode) addVoters() {
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

func (n *raftNode) waitForLeader(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.raft.Leader() != "" {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("cluster: timed out waiting for a leader")
}
