package wire

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

func randBytes(r *rand.Rand, max int) []byte {
	b := make([]byte, r.Intn(max+1))
	r.Read(b)
	return b
}

func TestFrameRoundTrip(t *testing.T) {
	prop := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var buf bytes.Buffer
		var want []Frame
		for range 1 + r.Intn(5) {
			f := Frame{Stream: r.Uint32(), Type: FrameType(1 + r.Intn(int(frameTypeEnd)-1)), Payload: randBytes(r, 300)}
			if err := WriteFrame(&buf, f.Stream, f.Type, f.Payload); err != nil {
				t.Logf("write: %v", err)
				return false
			}
			want = append(want, f)
		}
		for _, w := range want {
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Logf("read: %v", err)
				return false
			}
			if got.Stream != w.Stream || got.Type != w.Type || !bytes.Equal(got.Payload, w.Payload) {
				return false
			}
		}
		_, err := ReadFrame(&buf)
		return err == io.EOF
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}

func TestFrameLargePayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, 3*payloadChunk+17)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, 7, FrameRows, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := ReadFrame(&buf)
	if err != nil || !bytes.Equal(f.Payload, payload) {
		t.Fatalf("read large frame: err=%v, len=%d", err, len(f.Payload))
	}
}

func TestFrameRejectsMalformed(t *testing.T) {
	var ok bytes.Buffer
	if err := WriteFrame(&ok, 1, FrameQuery, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	enc := ok.Bytes()
	// Every strict prefix is an incomplete frame: io.EOF only for the empty one.
	for cut := range len(enc) {
		_, err := ReadFrame(bytes.NewReader(enc[:cut]))
		if cut == 0 && err != io.EOF {
			t.Errorf("empty input: err = %v, want io.EOF", err)
		}
		if cut > 0 && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("prefix %d/%d: err = %v, want io.ErrUnexpectedEOF", cut, len(enc), err)
		}
	}

	cases := map[string][]byte{
		"unknown type":      {0, 0, 0, 0, 0, 0, 0, 1, 0xee},
		"zero type":         {0, 0, 0, 0, 0, 0, 0, 1, 0},
		"oversized payload": {0xff, 0xff, 0xff, 0xff, 0, 0, 0, 1, byte(FrameRows)},
		// Claims far more than it delivers: must fail without trusting the length.
		"lying length": append([]byte{0x01, 0, 0, 0, 0, 0, 0, 1, byte(FrameRows)}, 1, 2, 3),
	}
	for name, b := range cases {
		if _, err := ReadFrame(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := WriteFrame(io.Discard, 1, FrameRows, make([]byte, MaxPayload+1)); err == nil {
		t.Error("writing an oversized payload: want an error")
	}
	if err := WriteFrame(io.Discard, 1, 0, nil); err == nil {
		t.Error("writing an unknown frame type: want an error")
	}
}

func TestHandshake(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHandshake(&buf); err != nil {
		t.Fatal(err)
	}
	if err := ReadHandshake(&buf); err != nil {
		t.Fatalf("matching handshake: %v", err)
	}

	other := []byte{'I', 'T', 'R', 'C', 0, byte(Version + 1), 0, 0}
	var ve *VersionError
	if err := ReadHandshake(bytes.NewReader(other)); !errors.As(err, &ve) || ve.Remote != Version+1 || ve.Local != Version {
		t.Errorf("version mismatch: err = %v", err)
	}
	for name, b := range map[string][]byte{
		"wrong magic": {'H', 'T', 'T', 'P', 0, byte(Version), 0, 0},
		"short":       {'I', 'T', 'R'},
		"empty":       {},
	} {
		err := ReadHandshake(bytes.NewReader(b))
		if err == nil || errors.As(err, &ve) {
			t.Errorf("%s: err = %v, want a protocol error", name, err)
		}
	}
}

func genRequest(r *rand.Rand) Request {
	return Request{
		Cypher:       string(randBytes(r, 80)),
		Params:       randBytes(r, 80),
		Write:        r.Intn(2) == 0,
		Linearizable: r.Intn(2) == 0,
		MaxRows:      r.Int63n(1 << 40),
		Timeout:      time.Duration(r.Int63n(1<<40)) * time.Millisecond,
	}
}

func TestRequestRoundTrip(t *testing.T) {
	prop := func(seed int64) bool {
		req := genRequest(rand.New(rand.NewSource(seed)))
		got, err := DecodeRequest(EncodeRequest(req))
		if err != nil {
			t.Logf("decode: %v", err)
			return false
		}
		return got.Cypher == req.Cypher && bytes.Equal(got.Params, req.Params) &&
			got.Write == req.Write && got.Linearizable == req.Linearizable &&
			got.MaxRows == req.MaxRows && got.Timeout == req.Timeout
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}

	// A sub-millisecond timeout must not become "no timeout".
	got, err := DecodeRequest(EncodeRequest(Request{Timeout: time.Microsecond}))
	if err != nil || got.Timeout != time.Millisecond {
		t.Errorf("sub-millisecond timeout decoded as %v (err %v), want 1ms", got.Timeout, err)
	}
	if got, err := DecodeRequest(EncodeRequest(Request{Timeout: -time.Second, MaxRows: -3})); err != nil || got.Timeout != 0 || got.MaxRows != 0 {
		t.Errorf("negative values decoded as %+v (err %v), want zeros", got, err)
	}
}

func TestColumnsRowsErrorRoundTrip(t *testing.T) {
	prop := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		cols := make([]string, r.Intn(6))
		for i := range cols {
			cols[i] = string(randBytes(r, 20))
		}
		gotCols, err := DecodeColumns(EncodeColumns(cols))
		if err != nil || !reflect.DeepEqual(gotCols, cols) {
			return false
		}

		rows := make([][]byte, r.Intn(8))
		for i := range rows {
			rows[i] = randBytes(r, 50)
		}
		gotRows, err := DecodeRows(EncodeRows(rows))
		if err != nil || len(gotRows) != len(rows) {
			return false
		}
		for i := range rows {
			if !bytes.Equal(rows[i], gotRows[i]) {
				return false
			}
		}

		e := &Error{Code: Code(r.Intn(5)), Msg: string(randBytes(r, 60))}
		gotErr, err := DecodeError(EncodeError(e))
		return err == nil && *gotErr == *e
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

func TestDecodersRejectMalformed(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	decoders := map[string]struct {
		enc    []byte
		decode func([]byte) error
	}{
		"request": {EncodeRequest(Request{Cypher: "MATCH (n) RETURN n", Params: []byte{1, 2, 3}, MaxRows: 9, Timeout: time.Second}),
			func(b []byte) error { _, err := DecodeRequest(b); return err }},
		"columns": {EncodeColumns([]string{"a", "bb", "ccc"}),
			func(b []byte) error { _, err := DecodeColumns(b); return err }},
		"rows": {EncodeRows([][]byte{randBytes(r, 9), {}, randBytes(r, 30)}),
			func(b []byte) error { _, err := DecodeRows(b); return err }},
		"error": {EncodeError(&Error{Code: CodeResultLimit, Msg: "too many rows"}),
			func(b []byte) error { _, err := DecodeError(b); return err }},
	}
	for name, d := range decoders {
		if err := d.decode(d.enc); err != nil {
			t.Errorf("%s: valid encoding rejected: %v", name, err)
		}
		for cut := range len(d.enc) {
			if err := d.decode(d.enc[:cut]); err == nil {
				t.Errorf("%s: a %d/%d-byte prefix decoded", name, cut, len(d.enc))
			}
		}
		if err := d.decode(append(append([]byte(nil), d.enc...), 0)); err == nil {
			t.Errorf("%s: a trailing byte was accepted", name)
		}
	}
	if _, err := DecodeRequest([]byte{0xf0, 0, 0, 0, 0}); err == nil {
		t.Error("request with unknown flags: want an error")
	}
	if _, err := DecodeRows([]byte{0xff, 0xff, 0xff, 0xff, 0x0f}); err == nil {
		t.Error("rows with a huge count: want an error")
	}
	if _, err := DecodeColumns([]byte{1, 0xff, 0xff, 0xff, 0xff, 0x0f, 'a'}); err == nil {
		t.Error("columns with a huge string: want an error")
	}
}

func TestChunkRows(t *testing.T) {
	row := func(n int) []byte { return bytes.Repeat([]byte{1}, n) }
	sizes := func(chunks [][][]byte) [][]int {
		out := make([][]int, len(chunks))
		for i, c := range chunks {
			for _, r := range c {
				out[i] = append(out[i], len(r))
			}
		}
		return out
	}
	cases := []struct {
		name string
		rows [][]byte
		want [][]int
	}{
		{"none", nil, [][]int{}},
		{"fits", [][]byte{row(10), row(20)}, [][]int{{10, 20}}},
		{"split", [][]byte{row(60), row(60), row(60)}, [][]int{{60}, {60}, {60}}},
		{"packs", [][]byte{row(30), row(30), row(30), row(30)}, [][]int{{30, 30, 30}, {30}}},
		{"oversized row travels alone", [][]byte{row(10), row(500), row(10)}, [][]int{{10}, {500}, {10}}},
	}
	for _, c := range cases {
		if got := sizes(ChunkRows(c.rows, 100)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: chunks = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestErrorIsAnError(t *testing.T) {
	var err error = &Error{Code: CodeResultLimit, Msg: "boom"}
	if err.Error() != "boom" {
		t.Errorf("Error() = %q", err.Error())
	}
	var we *Error
	if !errors.As(err, &we) || we.Code != CodeResultLimit {
		t.Error("errors.As failed")
	}
}

func FuzzReadFrame(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, 3, FrameQuery, []byte("hello"))
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		for {
			fr, err := ReadFrame(r)
			if err != nil {
				return
			}
			if len(fr.Payload) > len(data) {
				t.Fatalf("payload of %d bytes from %d bytes of input", len(fr.Payload), len(data))
			}
		}
	})
}

func FuzzDecodeMessages(f *testing.F) {
	f.Add(EncodeRequest(Request{Cypher: "RETURN 1", Params: []byte{1}, Write: true, MaxRows: 5, Timeout: time.Second}))
	f.Add(EncodeColumns([]string{"a", "b"}))
	f.Add(EncodeRows([][]byte{{1, 2}, {3}}))
	f.Add(EncodeError(&Error{Code: CodeResultLimit, Msg: "x"}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if req, err := DecodeRequest(data); err == nil {
			if again, err := DecodeRequest(EncodeRequest(req)); err != nil || again.Cypher != req.Cypher {
				t.Fatalf("request does not survive re-encoding: %v", err)
			}
		}
		if cols, err := DecodeColumns(data); err == nil {
			if again, err := DecodeColumns(EncodeColumns(cols)); err != nil || !reflect.DeepEqual(again, cols) {
				t.Fatalf("columns do not survive re-encoding: %v", err)
			}
		}
		if rows, err := DecodeRows(data); err == nil {
			if again, err := DecodeRows(EncodeRows(rows)); err != nil || len(again) != len(rows) {
				t.Fatalf("rows do not survive re-encoding: %v", err)
			}
		}
		if e, err := DecodeError(data); err == nil {
			if again, err := DecodeError(EncodeError(e)); err != nil || *again != *e {
				t.Fatalf("error does not survive re-encoding: %v", err)
			}
		}
	})
}
