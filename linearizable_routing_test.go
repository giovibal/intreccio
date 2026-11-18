package intreccio

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/giovibal/intreccio/internal/wire"
)

// steppingDown is a voter backend whose leadership answers come from a
// script, to put a leadership change exactly between two checks.
type steppingDown struct {
	*localBackend
	leader     []bool // successive IsLeader answers; the last one repeats
	calls      atomic.Int32
	barrierErr error
	barriers   atomic.Int32
	forwards   atomic.Int32
}

func (b *steppingDown) IsLeader() bool {
	i := int(b.calls.Add(1)) - 1
	if i >= len(b.leader) {
		i = len(b.leader) - 1
	}
	return b.leader[i]
}

func (b *steppingDown) Local() bool { return true }

func (b *steppingDown) ReadBarrier() error {
	b.barriers.Add(1)
	return b.barrierErr
}

func (b *steppingDown) Forward(_ context.Context, req wire.Request) ([]string, [][]byte, error) {
	b.forwards.Add(1)
	if !req.Linearizable {
		return nil, nil, errors.New("forwarded a read without its linearizable flag")
	}
	enc, err := encodeRows([][]any{{"leader"}})
	return []string{"src"}, enc, err
}

// TestLinearizableReadNeverSkipsTheBarrier covers #87: a voter that loses
// leadership between choosing a local read and running it must not serve a
// local snapshot without a barrier.
func TestLinearizableReadNeverSkipsTheBarrier(t *testing.T) {
	const q = "RETURN 'local' AS src"
	errNotLeader := errors.New("read barrier: node is not the leader")
	errDisk := errors.New("read barrier: log store failed")

	cases := []struct {
		name       string
		leader     []bool
		barrierErr error
		want       string // the row returned, or "" for an error
		wantErr    error
		forwards   int32
	}{
		{"leader throughout: barrier, then local read", []bool{true}, nil, "local", nil, 0},
		{"steps down after routing: the read goes to the new leader", []bool{true, false}, errNotLeader, "leader", nil, 1},
		{"barrier fails while still leader: the error is returned", []bool{true}, errDisk, "", errDisk, 0},
		{"not leader at routing: forwarded, no barrier", []bool{false}, nil, "leader", nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := openMemStore()
			if err != nil {
				t.Fatal(err)
			}
			be := &steppingDown{localBackend: &localBackend{store: store}, leader: tc.leader, barrierErr: tc.barrierErr}
			db := wrapBackend(be)
			defer func() { _ = db.Close() }()

			res, err := db.Query(context.Background(), q, nil, Linearizable())
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("Query: %v", err)
			case res.Rows[0][0] != tc.want:
				t.Fatalf("served by %v, want %s", res.Rows[0][0], tc.want)
			}
			if got := be.forwards.Load(); got != tc.forwards {
				t.Errorf("forwarded %d times, want %d", got, tc.forwards)
			}
			// A local answer is only ever given after a barrier.
			if res != nil && res.Rows[0][0] == "local" && be.barriers.Load() == 0 {
				t.Error("a linearizable read was served locally without a barrier")
			}
		})
	}

	// A default read stays local and never waits for a barrier.
	store, err := openMemStore()
	if err != nil {
		t.Fatal(err)
	}
	be := &steppingDown{localBackend: &localBackend{store: store}, leader: []bool{false}}
	db := wrapBackend(be)
	defer func() { _ = db.Close() }()
	if _, err := db.Query(context.Background(), q, nil); err != nil {
		t.Fatal(err)
	}
	if be.barriers.Load() != 0 || be.forwards.Load() != 0 {
		t.Errorf("default read: %d barriers, %d forwards, want none", be.barriers.Load(), be.forwards.Load())
	}
}
