package cluster

import (
	"errors"
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
	// applyFault, when set, runs inside each apply transaction; an error from
	// it fails that transaction as a storage failure would. Tests use it to
	// inject one; it is nil in production.
	applyFault atomic.Pointer[func() error]

	// failure is set once a committed entry could not be stored (see
	// failed). It is cleared only by a successful Restore.
	failure atomic.Pointer[error]
	// onFail, when set, is called once when the state machine fails. Set
	// before Raft starts.
	onFail func(error)
}

// ErrReplicaFailed is the error of a member whose state machine could not
// store a committed write (a disk failure, say). Raft does not deliver an
// entry twice, so the member's state is missing that write: it stops
// applying later ones, serving reads and taking snapshots, and gives up
// leadership. Restarting it recovers: Raft replays its log, from its last
// snapshot, into the state machine.
var ErrReplicaFailed = errors.New("cluster: this member failed to store a committed write and must be restarted")

// failed returns the state machine's failure, or nil while it is healthy.
func (f *fsm) failed() error {
	if p := f.failure.Load(); p != nil {
		return *p
	}
	return nil
}

var _ raft.FSM = (*fsm)(nil)

func newFSM(store storage.Store, snap storage.Snapshotter, tmpDir string, storeMu *sync.RWMutex) *fsm {
	return &fsm{store: store, snap: snap, tmpDir: tmpDir, storeMu: storeMu}
}

// Apply applies one committed log entry (a write-set) to the local store. It
// returns nil, or the error that failed the state machine: Raft hands it to
// the leader's ApplyFuture as the response, not as the future's error.
func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	if err := f.failed(); err != nil {
		// The state is missing an earlier write: applying this one on top
		// of it would build a state no other replica has.
		return err
	}
	if hook := f.beforeApply.Load(); hook != nil {
		(*hook)()
	}
	apply := func(txn storage.Txn) error {
		if err := applyWriteSet(l.Data, txn); err != nil {
			return err
		}
		if fault := f.applyFault.Load(); fault != nil {
			if err := (*fault)(); err != nil {
				return err
			}
		}
		// Recorded with the data it describes, so a reader always knows
		// exactly which index its snapshot reflects. A dataless client's
		// subscription relies on it to change voter without going backwards.
		return catalog.SetAppliedIndex(txn, l.Index)
	}
	var err error
	if hub := f.hub.Load(); hub != nil {
		// Commits through the store, then reports the write to the watchers.
		err = hub.Update(f.store, apply)
	} else {
		err = f.store.Update(apply)
	}
	if err != nil {
		err = fmt.Errorf("%w (entry %d: %v)", ErrReplicaFailed, l.Index, err)
		f.failure.Store(&err)
		if f.onFail != nil {
			f.onFail(err)
		}
		return err
	}
	return nil
}

// Snapshot captures the full store state at the current applied index by backing
// it up to a temp file. Streaming via a file (rather than an in-memory buffer)
// bounds memory for large stores while still reflecting state as of this call,
// per Raft's contract.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	if err := f.failed(); err != nil {
		// A snapshot of a state missing a write would let Raft drop the log
		// entries a restart needs to recover it.
		return nil, err
	}
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
	// A complete state from the leader replaces whatever this replica was
	// missing.
	f.failure.Store(nil)
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
