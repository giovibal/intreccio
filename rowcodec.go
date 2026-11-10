package intreccio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// The canonical row encoding (ADR 0012) serializes one result row — the public
// value model: null, bool, int64, float64, string, list, map, Node,
// Relationship — into a self-delimiting byte string. It is canonical: equal
// rows always produce identical bytes (map entries are written in key order),
// so an encoded row doubles as its identity. It is used wherever a row leaves
// the process or must be compared: query forwarding between cluster members
// and the result-set diff of live queries.
//
//	row          = uvarint(count) value*
//	value        = tag payload
//	null         = 0x00
//	false, true  = 0x01, 0x02
//	int          = 0x03 varint (zig-zag)
//	float        = 0x04 8 bytes, big-endian IEEE-754 bits
//	string       = 0x05 uvarint(len) bytes
//	list         = 0x06 uvarint(count) value*
//	map          = 0x07 uvarint(count) (uvarint(len) key value)*   keys ascending
//	node         = 0x08 varint(id) uvarint(count) (uvarint(len) label)* props
//	relationship = 0x09 varint(id) varint(start) varint(end) uvarint(len) type props
//	props        = uvarint(count) (uvarint(len) key value)*        keys ascending
//
// This is a wire and comparison format, unrelated to the on-disk record format
// of internal/storage/codec.
const (
	tagNull byte = iota
	tagFalse
	tagTrue
	tagInt
	tagFloat
	tagString
	tagList
	tagMap
	tagNode
	tagRelationship
)

// maxValueDepth bounds how deeply lists and maps may nest in an encoded value,
// so a corrupt or hostile input cannot exhaust the stack while decoding.
const maxValueDepth = 128

var errRowTruncated = errors.New("intreccio: row encoding: truncated input")

// encodeRows encodes each row separately. A nil result stays nil.
func encodeRows(rows [][]any) ([][]byte, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([][]byte, len(rows))
	for i, row := range rows {
		b, err := encodeRow(row)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// decodeRows is the inverse of encodeRows.
func decodeRows(enc [][]byte) ([][]any, error) {
	if enc == nil {
		return nil, nil
	}
	out := make([][]any, len(enc))
	for i, b := range enc {
		row, err := decodeRow(b)
		if err != nil {
			return nil, err
		}
		out[i] = row
	}
	return out, nil
}

// encodeRow returns the canonical encoding of one row.
func encodeRow(row []any) ([]byte, error) {
	dst := binary.AppendUvarint(nil, uint64(len(row)))
	var err error
	for _, v := range row {
		if dst, err = appendValue(dst, v, 0); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func appendValue(dst []byte, v any, depth int) ([]byte, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("intreccio: row encoding: value nested deeper than %d levels", maxValueDepth)
	}
	switch x := v.(type) {
	case nil:
		return append(dst, tagNull), nil
	case bool:
		if x {
			return append(dst, tagTrue), nil
		}
		return append(dst, tagFalse), nil
	case int:
		return binary.AppendVarint(append(dst, tagInt), int64(x)), nil
	case int64:
		return binary.AppendVarint(append(dst, tagInt), x), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(dst, tagFloat), math.Float64bits(x)), nil
	case string:
		return appendString(append(dst, tagString), x), nil
	case []any:
		dst = binary.AppendUvarint(append(dst, tagList), uint64(len(x)))
		var err error
		for _, el := range x {
			if dst, err = appendValue(dst, el, depth+1); err != nil {
				return nil, err
			}
		}
		return dst, nil
	case map[string]any:
		return appendEntries(append(dst, tagMap), x, depth)
	case Node:
		dst = binary.AppendVarint(append(dst, tagNode), x.ID)
		dst = binary.AppendUvarint(dst, uint64(len(x.Labels)))
		for _, l := range x.Labels {
			dst = appendString(dst, l)
		}
		return appendEntries(dst, x.Props, depth)
	case Relationship:
		dst = binary.AppendVarint(append(dst, tagRelationship), x.ID)
		dst = binary.AppendVarint(dst, x.Start)
		dst = binary.AppendVarint(dst, x.End)
		dst = appendString(dst, x.Type)
		return appendEntries(dst, x.Props, depth)
	default:
		return nil, fmt.Errorf("intreccio: row encoding: unsupported value type %T", v)
	}
}

func appendString(dst []byte, s string) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(s))), s...)
}

// appendEntries writes a map's entries in ascending key order.
func appendEntries(dst []byte, m map[string]any, depth int) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dst = binary.AppendUvarint(dst, uint64(len(keys)))
	var err error
	for _, k := range keys {
		dst = appendString(dst, k)
		if dst, err = appendValue(dst, m[k], depth+1); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// decodeRow is the inverse of encodeRow. It rejects truncated, trailing or
// otherwise malformed input instead of panicking, and never allocates more than
// the input could describe.
func decodeRow(b []byte) ([]any, error) {
	r := &rowReader{b: b}
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	row := make([]any, n)
	for i := range row {
		if row[i], err = r.value(0); err != nil {
			return nil, err
		}
	}
	if r.i != len(r.b) {
		return nil, fmt.Errorf("intreccio: row encoding: %d trailing bytes", len(r.b)-r.i)
	}
	return row, nil
}

type rowReader struct {
	b []byte
	i int
}

func (r *rowReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.b[r.i:])
	if n <= 0 {
		return 0, errRowTruncated
	}
	r.i += n
	return v, nil
}

func (r *rowReader) varint() (int64, error) {
	v, n := binary.Varint(r.b[r.i:])
	if n <= 0 {
		return 0, errRowTruncated
	}
	r.i += n
	return v, nil
}

// count reads an element count and checks it against the bytes left: every
// element takes at least one byte, so a larger count is corrupt.
func (r *rowReader) count() (int, error) {
	n, err := r.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(len(r.b)-r.i) {
		return 0, errRowTruncated
	}
	return int(n), nil
}

func (r *rowReader) str() (string, error) {
	n, err := r.count()
	if err != nil {
		return "", err
	}
	s := string(r.b[r.i : r.i+n])
	r.i += n
	return s, nil
}

func (r *rowReader) value(depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("intreccio: row encoding: value nested deeper than %d levels", maxValueDepth)
	}
	if r.i >= len(r.b) {
		return nil, errRowTruncated
	}
	tag := r.b[r.i]
	r.i++
	switch tag {
	case tagNull:
		return nil, nil
	case tagFalse:
		return false, nil
	case tagTrue:
		return true, nil
	case tagInt:
		return r.varint()
	case tagFloat:
		if len(r.b)-r.i < 8 {
			return nil, errRowTruncated
		}
		bits := binary.BigEndian.Uint64(r.b[r.i:])
		r.i += 8
		return math.Float64frombits(bits), nil
	case tagString:
		return r.str()
	case tagList:
		n, err := r.count()
		if err != nil {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = r.value(depth + 1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case tagMap:
		return r.entries(depth)
	case tagNode:
		id, err := r.varint()
		if err != nil {
			return nil, err
		}
		n, err := r.count()
		if err != nil {
			return nil, err
		}
		labels := make([]string, n)
		for i := range labels {
			if labels[i], err = r.str(); err != nil {
				return nil, err
			}
		}
		props, err := r.entries(depth)
		if err != nil {
			return nil, err
		}
		return Node{ID: id, Labels: labels, Props: props}, nil
	case tagRelationship:
		var (
			rel Relationship
			err error
		)
		if rel.ID, err = r.varint(); err != nil {
			return nil, err
		}
		if rel.Start, err = r.varint(); err != nil {
			return nil, err
		}
		if rel.End, err = r.varint(); err != nil {
			return nil, err
		}
		if rel.Type, err = r.str(); err != nil {
			return nil, err
		}
		if rel.Props, err = r.entries(depth); err != nil {
			return nil, err
		}
		return rel, nil
	default:
		return nil, fmt.Errorf("intreccio: row encoding: unknown value tag 0x%02x", tag)
	}
}

// entries reads a map, requiring strictly ascending keys: that is the only form
// the encoder produces, and accepting others would let two different byte
// strings stand for the same row.
func (r *rowReader) entries(depth int) (map[string]any, error) {
	n, err := r.count()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, n)
	prev := ""
	for i := range n {
		k, err := r.str()
		if err != nil {
			return nil, err
		}
		if i > 0 && k <= prev {
			return nil, errors.New("intreccio: row encoding: map keys are not in ascending order")
		}
		prev = k
		if out[k], err = r.value(depth + 1); err != nil {
			return nil, err
		}
	}
	return out, nil
}
