// Package wire is the framed protocol cluster members speak on the forwarding
// address (ADR 0011). One connection carries many logical streams:
//
//	connection = handshake frame*
//	handshake  = "ITRC" version(2, big-endian) reserved(2)
//	frame      = length(4, big-endian) stream(4, big-endian) type(1) payload
//
// Each side sends its handshake first, so members running incompatible
// versions fail at once and explicitly. A stream is opened by the dialling
// side with a request frame and answered by the other side until an End or
// Error frame closes it; stream 0 is reserved for connection-level frames
// (Ping / Pong).
//
// Two kinds of stream exist. A query stream is Query, then Result, Rows* and
// End. A subscription stream is Subscribe, then Result and an open-ended
// sequence of batches, each made of one or more Change frames and acknowledged
// by the subscriber with an Ack before the next is sent; the subscriber ends it
// with Cancel, the server with Error.
//
// The package holds only the encoding: it does no I/O scheduling and knows
// nothing about Cypher. Query parameters and result rows travel as opaque
// bytes in the root package's canonical row encoding (ADR 0012).
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the protocol version of this build. Bump it on any incompatible
// change to the handshake, the frame layout or a message encoding.
const Version uint16 = 1

// MaxPayload bounds the payload of one frame. Results are split over several
// Rows frames, so the bound applies to a single row and to a single request.
const MaxPayload = 32 << 20

// RowsChunkTarget is the payload size a Rows frame aims for.
const RowsChunkTarget = 256 << 10

// payloadChunk is how much of a payload is read at a time: a frame is never
// trusted to deliver the length it announces.
const payloadChunk = 64 << 10

const (
	headerLen    = 4 + 4 + 1
	handshakeLen = 8
)

var magic = [4]byte{'I', 'T', 'R', 'C'}

// ErrProtocol reports input that is not valid protocol data.
var ErrProtocol = errors.New("wire: protocol error")

// VersionError reports a peer speaking a different protocol version.
type VersionError struct {
	Local, Remote uint16
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("wire: protocol version mismatch (local %d, remote %d)", e.Local, e.Remote)
}

// FrameType identifies what a frame carries.
type FrameType byte

const (
	// FrameQuery opens a stream: its payload is a Request.
	FrameQuery FrameType = iota + 1
	// FrameResult starts a reply: its payload is the column names.
	FrameResult
	// FrameRows carries a chunk of result rows.
	FrameRows
	// FrameEnd closes a stream successfully.
	FrameEnd
	// FrameError closes a stream with an Error.
	FrameError
	// FrameCancel asks the peer to abandon a stream; no reply follows.
	FrameCancel
	// FramePing and FramePong are the keepalive, on stream 0.
	FramePing
	FramePong
	// FrameSubscribe opens a live-query stream: its payload is a
	// SubscribeRequest.
	FrameSubscribe
	// FrameChange carries a Batch, or part of one, of a live query.
	FrameChange
	// FrameAck tells the server the subscriber is ready for the next batch.
	FrameAck

	frameTypeEnd // one past the last known type
)

// Frame is one unit on the wire.
type Frame struct {
	Stream  uint32
	Type    FrameType
	Payload []byte
}

func (t FrameType) valid() bool { return t >= FrameQuery && t < frameTypeEnd }

// WriteFrame writes one frame with a single Write call, so concurrent writers
// only need to serialize calls to it.
func WriteFrame(w io.Writer, stream uint32, typ FrameType, payload []byte) error {
	if !typ.valid() {
		return fmt.Errorf("%w: unknown frame type %d", ErrProtocol, typ)
	}
	if len(payload) > MaxPayload {
		return fmt.Errorf("wire: frame payload of %d bytes exceeds the %d-byte limit", len(payload), MaxPayload)
	}
	buf := make([]byte, headerLen, headerLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:8], stream)
	buf[8] = byte(typ)
	_, err := w.Write(append(buf, payload...))
	return err
}

// ReadFrame reads one frame. It returns io.EOF when the stream ends cleanly
// between frames and io.ErrUnexpectedEOF when it ends inside one.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[0:4])
	f := Frame{Stream: binary.BigEndian.Uint32(hdr[4:8]), Type: FrameType(hdr[8])}
	if !f.Type.valid() {
		return Frame{}, fmt.Errorf("%w: unknown frame type %d", ErrProtocol, hdr[8])
	}
	if n > MaxPayload {
		return Frame{}, fmt.Errorf("%w: frame payload of %d bytes exceeds the %d-byte limit", ErrProtocol, n, MaxPayload)
	}
	// Grow the buffer as bytes actually arrive rather than allocating what the
	// header claims.
	for remaining := int(n); remaining > 0; {
		step := min(remaining, payloadChunk)
		start := len(f.Payload)
		f.Payload = append(f.Payload, make([]byte, step)...)
		if _, err := io.ReadFull(r, f.Payload[start:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
		remaining -= step
	}
	return f, nil
}

// WriteHandshake sends this build's handshake.
func WriteHandshake(w io.Writer) error {
	var b [handshakeLen]byte
	copy(b[:4], magic[:])
	binary.BigEndian.PutUint16(b[4:6], Version)
	_, err := w.Write(b[:])
	return err
}

// ReadHandshake reads the peer's handshake. It returns a *VersionError when the
// peer speaks another version, and an error wrapping ErrProtocol when the peer
// does not speak this protocol at all.
func ReadHandshake(r io.Reader) error {
	var b [handshakeLen]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return fmt.Errorf("%w: handshake: %v", ErrProtocol, err)
	}
	if [4]byte(b[:4]) != magic {
		return fmt.Errorf("%w: handshake: not an intreccio cluster endpoint", ErrProtocol)
	}
	if v := binary.BigEndian.Uint16(b[4:6]); v != Version {
		return &VersionError{Local: Version, Remote: v}
	}
	return nil
}

// Request asks a member to run a query. It is also the seam between the root
// package, which owns the Cypher pipeline, and the cluster layer, which only
// routes it.
type Request struct {
	Cypher string
	// Params is the query's parameter map in the canonical row encoding.
	Params []byte
	// Write marks a query that modifies data; it must run on the leader.
	Write bool
	// Linearizable asks for a read served by the leader behind a read barrier.
	Linearizable bool
	// MaxRows caps the result (0 = no cap), as intreccio.WithMaxRows does.
	MaxRows int64
	// Timeout bounds execution on the member that runs the query (0 = none).
	// It is relative, so clocks need not agree; it travels in milliseconds.
	Timeout time.Duration
}

const (
	flagWrite byte = 1 << iota
	flagLinearizable

	knownFlags = flagWrite | flagLinearizable
)

// EncodeRequest serializes r:
//
//	flags(1) uvarint(maxRows) uvarint(timeoutMs) string(cypher) bytes(params)
func EncodeRequest(r Request) []byte {
	var flags byte
	if r.Write {
		flags |= flagWrite
	}
	if r.Linearizable {
		flags |= flagLinearizable
	}
	var ms uint64
	if r.Timeout > 0 {
		// Round up: a deadline that is nearly here must not become "none".
		ms = uint64((r.Timeout + time.Millisecond - 1) / time.Millisecond)
	}
	b := make([]byte, 0, 1+2*binary.MaxVarintLen64+len(r.Cypher)+len(r.Params)+8)
	b = append(b, flags)
	b = binary.AppendUvarint(b, uint64(max(r.MaxRows, 0)))
	b = binary.AppendUvarint(b, ms)
	b = appendBytes(b, []byte(r.Cypher))
	return appendBytes(b, r.Params)
}

// DecodeRequest is the inverse of EncodeRequest.
func DecodeRequest(b []byte) (Request, error) {
	d := decoder{b: b}
	flags := d.byte()
	maxRows := d.uvarint()
	ms := d.uvarint()
	cypher := d.bytes()
	params := d.bytes()
	if err := d.finish("request"); err != nil {
		return Request{}, err
	}
	if flags&^knownFlags != 0 {
		return Request{}, fmt.Errorf("%w: request: unknown flags 0x%02x", ErrProtocol, flags)
	}
	const maxInt64 = 1<<63 - 1
	if maxRows > maxInt64 || ms > maxInt64/uint64(time.Millisecond) {
		return Request{}, fmt.Errorf("%w: request: value out of range", ErrProtocol)
	}
	return Request{
		Cypher:       string(cypher),
		Params:       params,
		Write:        flags&flagWrite != 0,
		Linearizable: flags&flagLinearizable != 0,
		MaxRows:      int64(maxRows),
		Timeout:      time.Duration(ms) * time.Millisecond,
	}, nil
}

// EncodeColumns serializes the column names of a result.
func EncodeColumns(cols []string) []byte {
	b := binary.AppendUvarint(nil, uint64(len(cols)))
	for _, c := range cols {
		b = appendBytes(b, []byte(c))
	}
	return b
}

// DecodeColumns is the inverse of EncodeColumns.
func DecodeColumns(b []byte) ([]string, error) {
	d := decoder{b: b}
	n := d.count()
	cols := make([]string, n)
	for i := range cols {
		cols[i] = string(d.bytes())
	}
	if err := d.finish("columns"); err != nil {
		return nil, err
	}
	return cols, nil
}

// EncodeRows serializes a chunk of rows, each already an opaque byte string.
func EncodeRows(rows [][]byte) []byte {
	size := binary.MaxVarintLen64
	for _, r := range rows {
		size += binary.MaxVarintLen64 + len(r)
	}
	b := binary.AppendUvarint(make([]byte, 0, size), uint64(len(rows)))
	for _, r := range rows {
		b = appendBytes(b, r)
	}
	return b
}

// DecodeRows is the inverse of EncodeRows. Decoded rows are never nil.
func DecodeRows(b []byte) ([][]byte, error) {
	d := decoder{b: b}
	n := d.count()
	rows := make([][]byte, n)
	for i := range rows {
		rows[i] = d.bytes()
	}
	if err := d.finish("rows"); err != nil {
		return nil, err
	}
	return rows, nil
}

// ChunkRows splits rows into consecutive chunks whose rows total at most
// target bytes each; a row larger than target travels alone.
func ChunkRows(rows [][]byte, target int) [][][]byte {
	var (
		chunks [][][]byte
		start  int
		size   int
	)
	for i, r := range rows {
		if i > start && size+len(r) > target {
			chunks = append(chunks, rows[start:i])
			start, size = i, 0
		}
		size += len(r)
	}
	if start < len(rows) {
		chunks = append(chunks, rows[start:])
	}
	return chunks
}

// Code classifies an Error so that the receiving side can map it back to a
// sentinel error instead of a bare message.
type Code uint32

const (
	// CodeGeneric is any error with no special meaning to the caller.
	CodeGeneric Code = iota
	// CodeResultLimit means the result exceeded Request.MaxRows.
	CodeResultLimit
	// CodeUnavailable means this member cannot serve the request right now
	// (it is shutting down, say); another member may.
	CodeUnavailable
	// CodeBehind means this member has not yet applied the index a subscriber
	// asked it to start after; another member may have.
	CodeBehind
	// The remaining codes name sentinel errors of the query itself, so that
	// errors.Is keeps working for the caller. Codes are part of the wire
	// format: never reuse or renumber one.
	CodeConflict
	CodeTxnTooLarge
	CodeIntegerOverflow
	CodeConstraintViolation
	CodeDeadlineExceeded
	CodeCanceled
)

// Error is a failure reported by the member that ran a request.
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// EncodeError serializes e.
func EncodeError(e *Error) []byte {
	return appendBytes(binary.AppendUvarint(nil, uint64(e.Code)), []byte(e.Msg))
}

// DecodeError is the inverse of EncodeError.
func DecodeError(b []byte) (*Error, error) {
	d := decoder{b: b}
	code := d.uvarint()
	msg := d.bytes()
	if err := d.finish("error"); err != nil {
		return nil, err
	}
	if code > 1<<32-1 {
		return nil, fmt.Errorf("%w: error: code out of range", ErrProtocol)
	}
	return &Error{Code: Code(code), Msg: string(msg)}, nil
}

func appendBytes(dst, b []byte) []byte {
	return append(binary.AppendUvarint(dst, uint64(len(b))), b...)
}

// decoder reads the fields of a message. The first failure sticks: every later
// read returns a zero value, and finish reports it.
type decoder struct {
	b   []byte
	i   int
	bad bool
}

func (d *decoder) byte() byte {
	if d.bad || d.i >= len(d.b) {
		d.bad = true
		return 0
	}
	v := d.b[d.i]
	d.i++
	return v
}

func (d *decoder) uvarint() uint64 {
	if d.bad {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.i:])
	if n <= 0 {
		d.bad = true
		return 0
	}
	d.i += n
	return v
}

// count reads an element count, checked against the bytes left: every element
// takes at least one byte.
func (d *decoder) count() int {
	n := d.uvarint()
	if d.bad || n > uint64(len(d.b)-d.i) {
		d.bad = true
		return 0
	}
	return int(n)
}

// bytes reads a length-prefixed byte string. The result is a copy and is never
// nil.
func (d *decoder) bytes() []byte {
	n := d.count()
	if d.bad {
		return nil
	}
	out := make([]byte, n)
	copy(out, d.b[d.i:d.i+n])
	d.i += n
	return out
}

func (d *decoder) finish(what string) error {
	if d.bad {
		return fmt.Errorf("%w: %s: truncated or malformed", ErrProtocol, what)
	}
	if d.i != len(d.b) {
		return fmt.Errorf("%w: %s: %d trailing bytes", ErrProtocol, what, len(d.b)-d.i)
	}
	return nil
}

// SubscribeRequest opens a live query on a member.
type SubscribeRequest struct {
	Request
	// AfterIndex, when non-zero, is the applied index of the last batch the
	// subscriber saw from another member. The serving member must have applied
	// at least that much before it answers, so the subscriber never sees the
	// result move backwards.
	AfterIndex uint64
}

// EncodeSubscribe serializes r: uvarint(afterIndex) bytes(request).
func EncodeSubscribe(r SubscribeRequest) []byte {
	return appendBytes(binary.AppendUvarint(nil, r.AfterIndex), EncodeRequest(r.Request))
}

// DecodeSubscribe is the inverse of EncodeSubscribe.
func DecodeSubscribe(b []byte) (SubscribeRequest, error) {
	d := decoder{b: b}
	after := d.uvarint()
	inner := d.bytes()
	if err := d.finish("subscribe"); err != nil {
		return SubscribeRequest{}, err
	}
	req, err := DecodeRequest(inner)
	if err != nil {
		return SubscribeRequest{}, err
	}
	return SubscribeRequest{Request: req, AfterIndex: after}, nil
}

// Batch is one batch of changes to a live query's result, with rows as opaque
// bytes. It mirrors intreccio.Change.
type Batch struct {
	// Initial marks a full result rather than a difference: every row is in
	// Added, in the order the query produced it.
	Initial bool
	// More marks a Change frame that does not complete its batch: the frames
	// that follow, up to one without More, belong to the same batch.
	More bool
	// Index is the applied index of the state the batch reflects.
	Index uint64
	// Added and Removed are the rows that entered and left the result.
	Added   [][]byte
	Removed [][]byte
}

const (
	batchInitial byte = 1 << iota
	batchMore

	knownBatchFlags = batchInitial | batchMore
)

// EncodeBatch serializes b: flags(1) uvarint(index) rows(added) rows(removed).
func EncodeBatch(b Batch) []byte {
	var flags byte
	if b.Initial {
		flags |= batchInitial
	}
	if b.More {
		flags |= batchMore
	}
	out := binary.AppendUvarint([]byte{flags}, b.Index)
	out = appendBytes(out, EncodeRows(b.Added))
	return appendBytes(out, EncodeRows(b.Removed))
}

// DecodeBatch is the inverse of EncodeBatch.
func DecodeBatch(b []byte) (Batch, error) {
	d := decoder{b: b}
	flags := d.byte()
	index := d.uvarint()
	added := d.bytes()
	removed := d.bytes()
	if err := d.finish("batch"); err != nil {
		return Batch{}, err
	}
	if flags&^knownBatchFlags != 0 {
		return Batch{}, fmt.Errorf("%w: batch: unknown flags 0x%02x", ErrProtocol, flags)
	}
	out := Batch{Initial: flags&batchInitial != 0, More: flags&batchMore != 0, Index: index}
	var err error
	if out.Added, err = DecodeRows(added); err != nil {
		return Batch{}, err
	}
	if out.Removed, err = DecodeRows(removed); err != nil {
		return Batch{}, err
	}
	return out, nil
}

// SplitBatch splits b into parts whose rows total at most target bytes each,
// so that a large batch — an initial result, typically — travels as several
// Change frames. Every part but the last has More set; removed rows come
// before added ones. A batch with no rows is still one part.
func SplitBatch(b Batch, target int) []Batch {
	var parts []Batch
	for _, chunk := range ChunkRows(b.Removed, target) {
		parts = append(parts, Batch{Initial: b.Initial, Index: b.Index, Removed: chunk})
	}
	for _, chunk := range ChunkRows(b.Added, target) {
		parts = append(parts, Batch{Initial: b.Initial, Index: b.Index, Added: chunk})
	}
	if len(parts) == 0 {
		parts = []Batch{{Initial: b.Initial, Index: b.Index}}
	}
	for i := range len(parts) - 1 {
		parts[i].More = true
	}
	return parts
}

// Merge appends the rows of part to b, which accumulates the parts of one
// batch as they arrive.
func (b *Batch) Merge(part Batch) {
	b.Initial = part.Initial
	b.Index = part.Index
	b.More = part.More
	b.Added = append(b.Added, part.Added...)
	b.Removed = append(b.Removed, part.Removed...)
}

// BatchSink is where a member serving a live query sends its output.
type BatchSink interface {
	// Columns sends the column names; it is called once, before any batch.
	Columns(cols []string) error
	// Batch sends one batch and returns once the subscriber is ready for the
	// next. While it blocks, further changes should be coalesced, not queued.
	Batch(b Batch) error
}

// BatchStream is the subscriber's end of a live query.
type BatchStream interface {
	// Recv returns the next batch, blocking until there is one. Calling it
	// signals that the previous batch has been consumed. It returns an error
	// when the stream has ended.
	Recv() (Batch, error)
}
