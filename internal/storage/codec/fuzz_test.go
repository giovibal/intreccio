package codec

import (
	"bytes"
	"testing"
)

// FuzzDecodeIndexValue asserts that DecodeIndexValue never panics on arbitrary
// bytes. It does not require successful decoding.
func FuzzDecodeIndexValue(f *testing.F) {
	seeds := [][]byte{
		nil,
		{0x00},                            // null
		{0x01, 0x01},                      // bool true
		{0x02, 0x80, 0, 0, 0, 0, 0, 0, 0}, // int 0
		{0x04, 0x00, 0x00},                // empty string
		{0x04, 'a', 'b', 0x00, 0x00},
		{0x04, 0x00, 0xFF, 0x00, 0x00}, // escaped \0
		{0xFF, 0xFF},
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = DecodeIndexValue(b)
	})
}

// FuzzIndexValueRoundTripInt asserts that any int64 round-trips through the
// order-preserving encoding without loss, and that the consumed byte count
// equals the encoded length.
func FuzzIndexValueRoundTripInt(f *testing.F) {
	for _, v := range []int64{0, 1, -1, 1<<31 - 1, -(1 << 31), 1<<62 + 1} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		enc, err := AppendIndexValue(nil, v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, n, err := DecodeIndexValue(enc)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if n != len(enc) {
			t.Fatalf("consumed %d, encoded %d", n, len(enc))
		}
		if got != v {
			t.Fatalf("round-trip int64: got %v, want %v", got, v)
		}
	})
}

// FuzzIndexValueRoundTripString verifies that round-trip preserves every byte
// of the input, including embedded NULs, and that decoding stops exactly at
// the terminator (any trailing suffix bytes are left untouched).
func FuzzIndexValueRoundTripString(f *testing.F) {
	for _, s := range []string{"", "a", "hello", "with\x00zero", string([]byte{0, 0xFF, 0})} {
		f.Add(s)
	}
	suffix := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	f.Fuzz(func(t *testing.T, s string) {
		enc, err := AppendIndexValue(nil, s)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		full := append(append([]byte{}, enc...), suffix...)
		got, n, err := DecodeIndexValue(full)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got != s {
			t.Fatalf("round-trip string: got %q, want %q", got, s)
		}
		if !bytes.Equal(full[n:], suffix) {
			t.Fatalf("trailing suffix consumed: %x", full[n:])
		}
	})
}
