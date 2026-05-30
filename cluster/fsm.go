package cluster

import (
	"bytes"
	"fmt"
	"io"

	"github.com/hashicorp/raft"

	"github.com/giovibal/mycypher/internal/storage"
)

// fsm is the Raft finite state machine. It is the single writer to the local
// store on every node: Apply replays a replicated write-set atomically via
// storage.Update, preserving the engine's index-consistency invariant (the
// write-set already contains every base record and index key produced by the
// leader's normal execution).
type fsm struct {
	store storage.Store
	snap  storage.Snapshotter
}

var _ raft.FSM = (*fsm)(nil)

func newFSM(store storage.Store, snap storage.Snapshotter) *fsm {
	return &fsm{store: store, snap: snap}
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

// Snapshot captures the full store state at the current applied index. The
// backup is buffered so Persist reflects state as of this call, per Raft's
// contract. (Streaming to a temp file is a Phase E hardening item.)
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	var buf bytes.Buffer
	if err := f.snap.Backup(&buf); err != nil {
		return nil, fmt.Errorf("fsm: snapshot: %w", err)
	}
	return &fsmSnapshot{data: buf.Bytes()}, nil
}

// Restore replaces the entire store contents with a snapshot.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()
	if err := f.snap.Load(rc); err != nil {
		return fmt.Errorf("fsm: restore: %w", err)
	}
	return nil
}

// fsmSnapshot holds a buffered backup ready to be written to a sink.
type fsmSnapshot struct{ data []byte }

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("fsm: persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() { s.data = nil }
