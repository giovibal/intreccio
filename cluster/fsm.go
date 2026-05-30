package cluster

import (
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/raft"

	"github.com/giovibal/mycypher/internal/storage"
)

// fsm is the Raft finite state machine. It is the single writer to the local
// store on every node: Apply replays a replicated write-set atomically via
// storage.Update, preserving the engine's index-consistency invariant (the
// write-set already contains every base record and index key produced by the
// leader's normal execution).
type fsm struct {
	store  storage.Store
	snap   storage.Snapshotter
	tmpDir string // where snapshot temp files are created
}

var _ raft.FSM = (*fsm)(nil)

func newFSM(store storage.Store, snap storage.Snapshotter, tmpDir string) *fsm {
	return &fsm{store: store, snap: snap, tmpDir: tmpDir}
}

// Apply applies one committed log entry (a write-set) to the local store.
func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	return f.store.Update(func(txn storage.Txn) error {
		return applyWriteSet(l.Data, txn)
	})
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

// Restore replaces the entire store contents with a snapshot.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	if err := f.snap.Load(rc); err != nil {
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
