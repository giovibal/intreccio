package changefeed

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/giovibal/intreccio/internal/storage"
	"github.com/giovibal/intreccio/internal/storage/codec"
)

// recordKeyLen is the length of an `n` or `e` key: the table tag plus an ID.
const recordKeyLen = 1 + 8

// Capture wraps the read-write transaction of one commit attempt and observes
// its writes to the record tables (`n`, `e`). Those two tables hold the whole
// logical state — every other key is an index or catalog entry derived from
// them — so they are all a ChangeSet needs.
//
// The first time a record is touched its before-image is read through the same
// transaction: one extra Get per touched record. A Capture belongs to a single
// attempt: Store.Update may run its function again on a conflict, and each run
// must start from a fresh Capture.
type Capture struct {
	real  storage.Txn
	recs  map[string]*images
	order []string // record keys in first-touch order, for a stable ChangeSet
}

// images holds the serialized record before the transaction and as last
// written by it; a nil slice with the flag unset means "absent".
type images struct {
	before, after       []byte
	hadBefore, hasAfter bool
}

var _ storage.Txn = (*Capture)(nil)

// NewCapture wraps real, which must be a read-write transaction.
func NewCapture(real storage.Txn) *Capture {
	return &Capture{real: real, recs: map[string]*images{}}
}

func (c *Capture) Get(key []byte) ([]byte, error) { return c.real.Get(key) }

func (c *Capture) Scan(prefix []byte) storage.Iterator { return c.real.Scan(prefix) }

func (c *Capture) Set(key, val []byte) error {
	if isRecordKey(key) {
		im, err := c.touch(key)
		if err != nil {
			return err
		}
		im.after, im.hasAfter = val, true
	}
	return c.real.Set(key, val)
}

func (c *Capture) Delete(key []byte) error {
	if isRecordKey(key) {
		im, err := c.touch(key)
		if err != nil {
			return err
		}
		im.after, im.hasAfter = nil, false
	}
	return c.real.Delete(key)
}

func isRecordKey(key []byte) bool {
	return len(key) == recordKeyLen && (key[0] == codec.KeyNode || key[0] == codec.KeyEdge)
}

// touch returns the images of key, reading the before-image on first use.
func (c *Capture) touch(key []byte) (*images, error) {
	if im, ok := c.recs[string(key)]; ok {
		return im, nil
	}
	im := &images{}
	before, err := c.real.Get(key)
	switch {
	case err == nil:
		im.before, im.hadBefore = before, true
	case !errors.Is(err, storage.ErrNotFound):
		return nil, fmt.Errorf("changefeed: read before-image: %w", err)
	}
	k := string(key)
	c.recs[k] = im
	c.order = append(c.order, k)
	return im, nil
}

// ChangeSet returns the net logical effect of the writes observed so far. Call
// it at the end of the transaction function, while the buffers handed to Set
// are still valid. A record left as it was — never existing before or after,
// or rewritten with identical bytes — is omitted; record encoding is
// deterministic, so byte equality is state equality.
//
// An error means a record could not be decoded. The transaction itself is
// unaffected; the caller should treat the commit as an unknown change.
func (c *Capture) ChangeSet() (*ChangeSet, error) {
	cs := &ChangeSet{}
	for _, k := range c.order {
		im := c.recs[k]
		if !im.hadBefore && !im.hasAfter {
			continue
		}
		if im.hadBefore && im.hasAfter && bytes.Equal(im.before, im.after) {
			continue
		}
		id := binary.BigEndian.Uint64([]byte(k[1:]))
		if k[0] == codec.KeyNode {
			ch := NodeChange{ID: id}
			if im.hadBefore {
				rec, err := codec.DecodeNode(im.before)
				if err != nil {
					return nil, fmt.Errorf("changefeed: node %d before-image: %w", id, err)
				}
				ch.Before = &rec
			}
			if im.hasAfter {
				rec, err := codec.DecodeNode(im.after)
				if err != nil {
					return nil, fmt.Errorf("changefeed: node %d after-image: %w", id, err)
				}
				ch.After = &rec
			}
			cs.Nodes = append(cs.Nodes, ch)
			continue
		}
		ch := EdgeChange{ID: id}
		if im.hadBefore {
			rec, err := codec.DecodeEdge(im.before)
			if err != nil {
				return nil, fmt.Errorf("changefeed: relationship %d before-image: %w", id, err)
			}
			ch.Before = &rec
		}
		if im.hasAfter {
			rec, err := codec.DecodeEdge(im.after)
			if err != nil {
				return nil, fmt.Errorf("changefeed: relationship %d after-image: %w", id, err)
			}
			ch.After = &rec
		}
		cs.Edges = append(cs.Edges, ch)
	}
	return cs, nil
}
