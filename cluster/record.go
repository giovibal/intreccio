package cluster

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
)

// errStaged is returned by the staging transaction to force the underlying
// engine to discard (roll back) without committing. The write-set captured by
// recordTxn is replicated through Raft and applied by the FSM instead, so the
// FSM is the single writer on every node (leader included).
var errStaged = errors.New("cluster: staged (rolled back, replicated via raft)")

// opKind distinguishes the two mutation kinds in a write-set.
type opKind byte

const (
	opSet opKind = 1
	opDel opKind = 2
)

// op is a single recorded mutation.
type op struct {
	kind opKind
	key  []byte
	val  []byte // nil for opDel
}

// recordTxn wraps a real read-write storage.Txn. Reads pass straight through to
// the underlying transaction — which, because Set/Delete are also applied to it,
// yields read-your-writes within the statement for free (Badger merges pending
// writes into Get and Scan). Every mutation is additionally recorded so the
// ordered effect set can be replicated.
type recordTxn struct {
	real storage.Txn
	ops  []op
}

var _ storage.Txn = (*recordTxn)(nil)

func newRecordTxn(real storage.Txn) *recordTxn {
	return &recordTxn{real: real}
}

func (r *recordTxn) Get(key []byte) ([]byte, error) { return r.real.Get(key) }

func (r *recordTxn) Scan(prefix []byte) storage.Iterator { return r.real.Scan(prefix) }

func (r *recordTxn) ScanFrom(prefix, start []byte) storage.Iterator {
	return r.real.ScanFrom(prefix, start)
}

func (r *recordTxn) Set(key, val []byte) error {
	r.ops = append(r.ops, op{kind: opSet, key: clone(key), val: clone(val)})
	return r.real.Set(key, val)
}

func (r *recordTxn) Delete(key []byte) error {
	r.ops = append(r.ops, op{kind: opDel, key: clone(key)})
	return r.real.Delete(key)
}

// empty reports whether the transaction recorded no mutations.
func (r *recordTxn) empty() bool { return len(r.ops) == 0 }

// encode serializes the recorded write-set. Layout:
//
//	uvarint(count)
//	repeated: kind(1) | uvarint(keyLen) | key | [uvarint(valLen) | val] (set only)
func (r *recordTxn) encode() []byte {
	var buf bytes.Buffer
	var scratch [binary.MaxVarintLen64]byte
	putUvarint := func(x uint64) {
		n := binary.PutUvarint(scratch[:], x)
		buf.Write(scratch[:n])
	}
	putUvarint(uint64(len(r.ops)))
	for _, o := range r.ops {
		buf.WriteByte(byte(o.kind))
		putUvarint(uint64(len(o.key)))
		buf.Write(o.key)
		if o.kind == opSet {
			putUvarint(uint64(len(o.val)))
			buf.Write(o.val)
		}
	}
	return buf.Bytes()
}

// applyWriteSet decodes a write-set and applies it to txn in order. It is the
// inverse of recordTxn.encode and the only mutation path executed by the FSM.
func applyWriteSet(data []byte, txn storage.Txn) error {
	r := bytes.NewReader(data)
	count, err := binary.ReadUvarint(r)
	if err != nil {
		return fmt.Errorf("write-set: count: %w", err)
	}
	for i := range count {
		kind, err := r.ReadByte()
		if err != nil {
			return fmt.Errorf("write-set: op %d kind: %w", i, err)
		}
		key, err := readBytes(r)
		if err != nil {
			return fmt.Errorf("write-set: op %d key: %w", i, err)
		}
		switch opKind(kind) {
		case opSet:
			val, err := readBytes(r)
			if err != nil {
				return fmt.Errorf("write-set: op %d val: %w", i, err)
			}
			if err := txn.Set(key, val); err != nil {
				return err
			}
		case opDel:
			if err := txn.Delete(key); err != nil {
				return err
			}
		default:
			return fmt.Errorf("write-set: op %d unknown kind %d", i, kind)
		}
	}
	return nil
}

func readBytes(r *bytes.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := readFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func readFull(r *bytes.Reader, b []byte) (int, error) {
	read := 0
	for read < len(b) {
		n, err := r.Read(b[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// errStaleWrite is the state machine's answer to an entry staged on a state
// that changed before the entry was applied (see encodeBase). It is decided
// from the replicated state alone, so every replica refuses the same entries
// and stays identical.
var errStaleWrite = errors.New("cluster: write staged on a stale state")

// errEntryRefused is the state machine's answer to an entry it cannot apply
// for a reason found in the entry itself (a malformed extension): every
// replica refuses it alike and writes nothing.
var errEntryRefused = errors.New("cluster: log entry refused")

// applyResponse interprets the state machine's response to a write-set
// entry this leader proposed and Raft committed:
//
//   - nil: applied.
//   - errStaleWrite: refused everywhere because the state it was staged on
//     changed; nothing was written and the write may be staged again
//     (rejected is true).
//   - errEntryRefused: refused everywhere; nothing was written, but staging
//     it again would not help.
//   - anything else, ErrReplicaFailed included: this member failed to store
//     an entry the other replicas apply, so the write took effect but cannot
//     be confirmed here (ErrWriteOutcomeUnknown).
func applyResponse(resp any) (rejected bool, err error) {
	respErr, _ := resp.(error)
	switch {
	case respErr == nil:
		return false, nil
	case errors.Is(respErr, errStaleWrite):
		return true, nil
	case errors.Is(respErr, errEntryRefused):
		return false, respErr
	default:
		return false, unknownOutcome("committed, but this member failed to store it: %v", respErr)
	}
}

// extBaseV1 tags the version-1 extension of a write-set entry.
const extBaseV1 byte = 1

// encodeBase is the Raft log extension of a write-set: the applied index
// (catalog.AppliedIndex) of the state the write was staged on. Layout:
//
//	extBaseV1 | uvarint(base)
//
// It travels in raft.Log.Extensions, next to the unchanged write-set in Data,
// so entries written before it existed still decode.
func encodeBase(base uint64) []byte {
	b := make([]byte, 1, 1+binary.MaxVarintLen64)
	b[0] = extBaseV1
	return binary.AppendUvarint(b, base)
}

// decodeBase reads encodeBase. ok is false for an entry with no extension,
// which carries no base to check.
func decodeBase(ext []byte) (base uint64, ok bool, err error) {
	if len(ext) == 0 {
		return 0, false, nil
	}
	if ext[0] != extBaseV1 {
		return 0, false, fmt.Errorf("write-set: unknown extension version %d", ext[0])
	}
	base, n := binary.Uvarint(ext[1:])
	if n <= 0 || 1+n != len(ext) {
		return 0, false, errors.New("write-set: malformed extension")
	}
	return base, true, nil
}
