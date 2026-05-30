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
