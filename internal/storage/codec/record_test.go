package codec

import (
	"reflect"
	"testing"
)

func TestNodeRecordRoundTrip(t *testing.T) {
	cases := []NodeRecord{
		{Labels: nil, Props: map[uint32]any{}},
		{Labels: []uint32{1}, Props: map[uint32]any{}},
		{Labels: []uint32{3, 1, 2}, Props: map[uint32]any{
			10: "Alice",
			11: int64(30),
			12: true,
			13: 1.5,
			14: nil,
			15: []any{int64(1), "two", 3.0},
			16: map[string]any{"city": "Rome", "zip": int64(100)},
		}},
	}
	for i, in := range cases {
		b, err := EncodeNode(in)
		if err != nil {
			t.Fatalf("case %d: EncodeNode: %v", i, err)
		}
		got, err := DecodeNode(b)
		if err != nil {
			t.Fatalf("case %d: DecodeNode: %v", i, err)
		}
		// Labels are sorted ascending during encode.
		wantLabels := sortedCopy(in.Labels)
		if !reflect.DeepEqual(got.Labels, wantLabels) {
			t.Errorf("case %d: labels got %v want %v", i, got.Labels, wantLabels)
		}
		if !reflect.DeepEqual(got.Props, in.Props) {
			t.Errorf("case %d: props got %#v want %#v", i, got.Props, in.Props)
		}
	}
}

func TestEdgeRecordRoundTrip(t *testing.T) {
	in := EdgeRecord{
		Type: 7,
		Src:  0xAABBCCDD,
		Dst:  0x11223344,
		Props: map[uint32]any{
			1: "since",
			2: int64(-2020),
		},
	}
	b, err := EncodeEdge(in)
	if err != nil {
		t.Fatalf("EncodeEdge: %v", err)
	}
	got, err := DecodeEdge(b)
	if err != nil {
		t.Fatalf("DecodeEdge: %v", err)
	}
	if got.Type != in.Type || got.Src != in.Src || got.Dst != in.Dst {
		t.Errorf("header got (%d,%d,%d) want (%d,%d,%d)", got.Type, got.Src, got.Dst, in.Type, in.Src, in.Dst)
	}
	if !reflect.DeepEqual(got.Props, in.Props) {
		t.Errorf("props got %#v want %#v", got.Props, in.Props)
	}
}

// The encoding must be deterministic (sorted keys) regardless of the map
// iteration order.
func TestEncodeDeterministic(t *testing.T) {
	rec := NodeRecord{Labels: []uint32{2, 1}, Props: map[uint32]any{
		5: "x", 1: int64(1), 9: true, 3: 2.0,
	}}
	first, err := EncodeNode(rec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		b, err := EncodeNode(rec)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, b) {
			t.Fatal("non-deterministic encoding")
		}
	}
}

func sortedCopy(in []uint32) []uint32 {
	out := append([]uint32(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	if out == nil {
		out = []uint32{}
	}
	return out
}
