package codec

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func encodeIndex(t *testing.T, v any) []byte {
	t.Helper()
	b, err := AppendIndexValue(nil, v)
	if err != nil {
		t.Fatalf("AppendIndexValue(%v): %v", v, err)
	}
	return b
}

func TestIndexValueRoundTrip(t *testing.T) {
	cases := []any{
		nil,
		true, false,
		int64(0), int64(1), int64(-1),
		int64(math.MaxInt64), int64(math.MinInt64),
		float64(0), math.Copysign(0, -1), 1.5, -1.5,
		math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64,
		"", "a", "hello", "with\x00zero", "\x00\x00\xff",
	}
	for _, want := range cases {
		enc := encodeIndex(t, want)
		got, n, err := DecodeIndexValue(enc)
		if err != nil {
			t.Fatalf("DecodeIndexValue(%v): %v", want, err)
		}
		if n != len(enc) {
			t.Errorf("%v: consumed %d bytes, expected %d", want, n, len(enc))
		}
		if !valueEqual(got, want) {
			t.Errorf("round-trip: got %v (%T), want %v (%T)", got, got, want, want)
		}
	}
}

// The value in `p` keys is followed by the nodeID: decoding must consume exactly
// the value portion and leave the rest.
func TestIndexValueBoundaryWithSuffix(t *testing.T) {
	suffix := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03}
	for _, v := range []any{nil, true, int64(-42), 3.14, "edge\x00case"} {
		enc := encodeIndex(t, v)
		full := append(append([]byte{}, enc...), suffix...)
		got, n, err := DecodeIndexValue(full)
		if err != nil {
			t.Fatalf("decode %v: %v", v, err)
		}
		if n != len(enc) {
			t.Errorf("%v: consumed %d, expected %d", v, n, len(enc))
		}
		if !valueEqual(got, v) {
			t.Errorf("%v: got %v", v, got)
		}
		if !bytes.Equal(full[n:], suffix) {
			t.Errorf("%v: wrong leftover suffix: %x", v, full[n:])
		}
	}
}

func TestIndexValueOrderingInt(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	vals := make([]int64, 500)
	for i := range vals {
		vals[i] = int64(r.Uint64())
	}
	checkOrdering(t, len(vals),
		func(i int) any { return vals[i] },
		func(i, j int) int { return cmpInt(vals[i], vals[j]) })
}

func TestIndexValueOrderingFloat(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	vals := make([]float64, 500)
	for i := range vals {
		vals[i] = (r.Float64() - 0.5) * math.MaxFloat64
	}
	checkOrdering(t, len(vals),
		func(i int) any { return vals[i] },
		func(i, j int) int { return cmpFloat(vals[i], vals[j]) })
}

func TestIndexValueOrderingString(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	vals := make([]string, 500)
	for i := range vals {
		n := r.Intn(12)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte(r.Intn(256)) // include 0x00 to exercise the escape
		}
		vals[i] = string(b)
	}
	checkOrdering(t, len(vals),
		func(i int) any { return vals[i] },
		func(i, j int) int { return bytes.Compare([]byte(vals[i]), []byte(vals[j])) })
}

func TestIndexValueOrderingBool(t *testing.T) {
	if bytes.Compare(encodeIndex(t, false), encodeIndex(t, true)) >= 0 {
		t.Error("false must order before true")
	}
}

// Order by type band: NULL < BOOL < INT < FLOAT < STRING.
func TestIndexValueOrderingCrossType(t *testing.T) {
	ordered := []any{nil, false, true, int64(math.MaxInt64), -math.MaxFloat64, math.MaxFloat64, "", "zzz"}
	for i := 0; i+1 < len(ordered); i++ {
		a := encodeIndex(t, ordered[i])
		b := encodeIndex(t, ordered[i+1])
		if bytes.Compare(a, b) >= 0 {
			t.Errorf("expected enc(%v) < enc(%v)", ordered[i], ordered[i+1])
		}
	}
}

// checkOrdering verifies that sorting by bytes matches the logical order,
// comparing every pair.
func checkOrdering(t *testing.T, n int, val func(int) any, cmp func(i, j int) int) {
	t.Helper()
	enc := make([][]byte, n)
	for i := 0; i < n; i++ {
		enc[i] = encodeIndex(t, val(i))
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return bytes.Compare(enc[idx[a]], enc[idx[b]]) < 0 })
	for k := 0; k+1 < len(idx); k++ {
		i, j := idx[k], idx[k+1]
		if cmp(i, j) > 0 {
			t.Fatalf("byte order inconsistent with logical order between %v and %v", val(i), val(j))
		}
	}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func valueEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y)))
	default:
		return a == b
	}
}
