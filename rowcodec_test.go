package intreccio

import (
	"bytes"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// genValue builds a random result value. Lists and maps are always non-nil and
// floats never NaN, so reflect.DeepEqual is a valid round-trip oracle.
func genValue(r *rand.Rand, depth int) any {
	max := 10
	if depth <= 0 {
		max = 6 // scalars only
	}
	switch r.Intn(max) {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		return r.Int63() - r.Int63()
	case 3:
		return math.Float64frombits(r.Uint64()&^(0x7ff<<52) | uint64(r.Intn(0x7ff))<<52)
	case 4, 5:
		return genString(r)
	case 6:
		out := make([]any, r.Intn(4))
		for i := range out {
			out[i] = genValue(r, depth-1)
		}
		return out
	case 7:
		return genMap(r, depth-1)
	case 8:
		labels := make([]string, r.Intn(3))
		for i := range labels {
			labels[i] = genString(r)
		}
		return Node{ID: r.Int63(), Labels: labels, Props: genMap(r, depth-1)}
	default:
		return Relationship{ID: r.Int63(), Type: genString(r), Start: r.Int63(), End: r.Int63(), Props: genMap(r, depth-1)}
	}
}

func genString(r *rand.Rand) string {
	b := make([]byte, r.Intn(6))
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return string(b)
}

func genMap(r *rand.Rand, depth int) map[string]any {
	out := make(map[string]any)
	for range r.Intn(4) {
		out[genString(r)] = genValue(r, depth)
	}
	return out
}

func genRow(r *rand.Rand) []any {
	row := make([]any, r.Intn(5))
	for i := range row {
		row[i] = genValue(r, 3)
	}
	return row
}

func TestRowCodecRoundTrip(t *testing.T) {
	prop := func(seed int64) bool {
		row := genRow(rand.New(rand.NewSource(seed)))
		enc, err := encodeRow(row)
		if err != nil {
			t.Logf("encode: %v", err)
			return false
		}
		dec, err := decodeRow(enc)
		if err != nil {
			t.Logf("decode: %v", err)
			return false
		}
		return reflect.DeepEqual(row, dec)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestRowCodecCanonical checks that equal rows encode to identical bytes no
// matter how their maps were built, so the encoding can serve as a row key.
func TestRowCodecCanonical(t *testing.T) {
	prop := func(seed int64) bool {
		a, err := encodeRow(genRow(rand.New(rand.NewSource(seed))))
		if err != nil {
			return false
		}
		b, err := encodeRow(genRow(rand.New(rand.NewSource(seed))))
		if err != nil {
			return false
		}
		return bytes.Equal(a, b)
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}

	big := map[string]any{}
	for i := range 64 {
		big[string(rune('a'+i%26))+string(rune('A'+i/26))] = int64(i)
	}
	first, err := encodeRow([]any{big})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for range 20 {
		again, err := encodeRow([]any{big})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("map encoding depends on iteration order")
		}
	}
}

// TestRowCodecDistinguishesTypes checks that values that print alike but differ
// in type do not collide.
func TestRowCodecDistinguishesTypes(t *testing.T) {
	rows := [][]any{
		{int64(1)}, {float64(1)}, {"1"}, {true}, {nil}, {[]any{}}, {map[string]any{}},
		{[]any{int64(1)}}, {int64(1), nil}, {},
		{Node{ID: 1, Props: map[string]any{}}},
		{Relationship{ID: 1, Props: map[string]any{}}},
	}
	seen := map[string]int{}
	for i, row := range rows {
		enc, err := encodeRow(row)
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if j, dup := seen[string(enc)]; dup {
			t.Errorf("rows %d and %d encode identically", j, i)
		}
		seen[string(enc)] = i
	}
}

func TestRowCodecNormalizes(t *testing.T) {
	enc, err := encodeRow([]any{7, Node{ID: 1}, []any(nil), map[string]any(nil), math.NaN()})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := decodeRow(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec[0] != int64(7) {
		t.Errorf("int decoded as %T(%v), want int64(7)", dec[0], dec[0])
	}
	if n := dec[1].(Node); n.Props == nil || n.Labels == nil {
		t.Errorf("node = %#v, want non-nil Labels and Props", n)
	}
	if l, ok := dec[2].([]any); !ok || l == nil {
		t.Errorf("nil list decoded as %#v", dec[2])
	}
	if m, ok := dec[3].(map[string]any); !ok || m == nil {
		t.Errorf("nil map decoded as %#v", dec[3])
	}
	if f, ok := dec[4].(float64); !ok || !math.IsNaN(f) {
		t.Errorf("NaN decoded as %#v", dec[4])
	}
}

func TestRowCodecRejectsUnsupported(t *testing.T) {
	for _, v := range []any{int32(1), uint(1), float32(1), []string{"a"}, struct{}{}, map[string]int{}} {
		if _, err := encodeRow([]any{v}); err == nil {
			t.Errorf("encode %T: want an error", v)
		}
	}
	deep := any(nil)
	for range maxValueDepth + 1 {
		deep = []any{deep}
	}
	if _, err := encodeRow([]any{deep}); err == nil {
		t.Error("encode beyond the nesting limit: want an error")
	}
}

// TestRowCodecRejectsMalformed feeds every strict prefix of valid encodings and
// a few hand-made corruptions: all must fail cleanly.
func TestRowCodecRejectsMalformed(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for range 200 {
		enc, err := encodeRow(genRow(r))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		for cut := range len(enc) {
			if _, err := decodeRow(enc[:cut]); err == nil {
				t.Fatalf("decode of a %d/%d-byte prefix succeeded", cut, len(enc))
			}
		}
		if _, err := decodeRow(append(append([]byte(nil), enc...), 0)); err == nil {
			t.Fatal("decode with a trailing byte succeeded")
		}
	}

	cases := map[string][]byte{
		"empty":             {},
		"unknown tag":       {1, 0xff},
		"huge row count":    {0xff, 0xff, 0xff, 0xff, 0x0f},
		"huge list count":   {1, tagList, 0xff, 0xff, 0xff, 0xff, 0x0f},
		"huge string":       {1, tagString, 0xff, 0xff, 0xff, 0xff, 0x0f, 'a'},
		"unsorted map keys": {1, tagMap, 2, 1, 'b', tagNull, 1, 'a', tagNull},
		"repeated map key":  {1, tagMap, 2, 1, 'a', tagNull, 1, 'a', tagNull},
	}
	for name, b := range cases {
		if _, err := decodeRow(b); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	deep := bytes.Repeat([]byte{tagList, 1}, maxValueDepth+1)
	deep = append([]byte{1}, append(deep, tagNull)...)
	if _, err := decodeRow(deep); err == nil {
		t.Error("decode beyond the nesting limit: want an error")
	}
}

func TestRowsCodec(t *testing.T) {
	rows := [][]any{{int64(1), "a"}, {nil, Node{ID: 2, Labels: []string{"L"}, Props: map[string]any{"k": true}}}}
	enc, err := encodeRows(rows)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := decodeRows(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(rows, dec) {
		t.Errorf("round trip = %#v, want %#v", dec, rows)
	}
	if enc, err := encodeRows(nil); err != nil || enc != nil {
		t.Errorf("encodeRows(nil) = %v, %v; want nil, nil", enc, err)
	}
	if dec, err := decodeRows(nil); err != nil || dec != nil {
		t.Errorf("decodeRows(nil) = %v, %v; want nil, nil", dec, err)
	}
}

// FuzzDecodeRow checks that arbitrary bytes never panic the decoder and that
// anything it accepts re-encodes to a stable form.
func FuzzDecodeRow(f *testing.F) {
	r := rand.New(rand.NewSource(7))
	for range 20 {
		enc, err := encodeRow(genRow(r))
		if err != nil {
			f.Fatalf("encode: %v", err)
		}
		f.Add(enc)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		row, err := decodeRow(data)
		if err != nil {
			return
		}
		enc, err := encodeRow(row)
		if err != nil {
			t.Fatalf("re-encode of a decoded row failed: %v", err)
		}
		again, err := decodeRow(enc)
		if err != nil {
			t.Fatalf("decode of a re-encoded row failed: %v", err)
		}
		enc2, err := encodeRow(again)
		if err != nil || !bytes.Equal(enc, enc2) {
			t.Fatalf("re-encoding is not stable (err=%v)", err)
		}
	})
}
