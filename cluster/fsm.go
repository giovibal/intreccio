package cluster

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/raft"

	"github.com/giovibal/intreccio/internal/catalog"
	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/storage"
)

// fsm is the Raft finite state machine. It is the single writer to the local
// store on every node: Apply replays a replicated write-set atomically via
// storage.Update, preserving the engine's index-consistency invariant (the
// write-set already contains every base record and index key produced by the
// leader's normal execution).
//
// It is also the clustered commit point for live queries (ADR 0010): because
// every node applies every committed write here, each voter can serve its own
// watchers from its own apply stream, whoever the leader is.
type fsm struct {
	store  storage.Store
	snap   storage.Snapshotter
	tmpDir string // where snapshot temp files are created
	// storeMu guards the store against a Restore (Badger DropAll+Load, which is
	// unsafe concurrent with any transaction) running while application reads are
	// in flight. Restore takes it exclusively; reads take it shared. Shared with
	// the owning Node. Apply and Snapshot run on Raft's single FSM goroutine, so
	// they never overlap Restore and do not need it.
	storeMu *sync.RWMutex
	// hub receives every applied write and every restore. It is set once the
	// owning DB exists, which may be after Raft has started replaying the log,
	// hence the atomic pointer; nil means nobody can be watching yet.
	hub atomic.Pointer[changefeed.Hub]
	// beforeApply, when set, runs before each command is applied. Tests use it
	// to make a replica's state machine lag behind its log; it is nil in
	// production.
	beforeApply atomic.Pointer[func()]
}

var _ raft.FSM = (*fsm)(nil)

func newFSM(store storage.Store, snap storage.Snapshotter, tmpDir string, storeMu *sync.RWMutex) *fsm {
	return &fsm{store: store, snap: snap, tmpDir: tmpDir, storeMu: storeMu}
}

// Apply applies one committed log entry (a write-set) to the local store.
func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	if hook := f.beforeApply.Load(); hook != nil {
		(*hook)()
	}
	apply := func(txn storage.Txn) error {
		if err := applyWriteSet(l.Data, txn); err != nil {
			return err
		}
		// Recorded with the data it describes, so a reader always knows
		// exactly which index its snapshot reflects. A dataless client's
		// subscription relies on it to change voter without going backwards.
		return catalog.SetAppliedIndex(txn, l.Index)
	}
	if hub := f.hub.Load(); hub != nil {
		// Commits through the store, then reports the write to the watchers.
		return hub.Update(f.store, apply)
	}
	return f.store.Update(apply)
}

// Snapshot captures the full store state at the current applied index by backing
// it up to a temp file. Streaming via a file (rather than an in-memory buffer)
// bounds memory for large stores while still reflecting state as of this call,
// per Raft's contract.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	tmp, err := os.CreateTemp(f.tmpDir, "snapshot-*.bak")
	if err != nil {
		return nil, fmt.Errorf("fsm: snapshot temp: %w", err)
	}
	if err := f.snap.Backup(tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("fsm: snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("fsm: snapshot close: %w", err)
	}
	return &fsmSnapshot{path: tmp.Name()}, nil
}

// Restore replaces the entire store contents with a snapshot. It takes the store
// lock exclusively so no read transaction is open during Badger's DropAll+Load.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	f.storeMu.Lock()
	err := f.snap.Load(rc)
	f.storeMu.Unlock()
	// The state was replaced wholesale, with no write-set to describe it — and
	// a failed load leaves it empty or partial. Either way every watcher must
	// re-evaluate; this runs after the lock is released because doing so reads
	// the store.
	if hub := f.hub.Load(); hub != nil {
		hub.Invalidate()
	}
	if err != nil {
		return fmt.Errorf("fsm: restore: %w", err)
	}
	return nil
}

// fsmSnapshot streams a backup file to the snapshot sink.
type fsmSnapshot struct{ path string }

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	f, err := os.Open(s.path)
	if err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("fsm: open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(sink, f); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("fsm: persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() { _ = os.Remove(s.path) }
