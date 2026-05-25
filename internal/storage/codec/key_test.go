package codec

import (
	"bytes"
	"testing"
)

func TestOutKeyRoundTrip(t *testing.T) {
	src, typ, dst, edge := uint64(7), uint32(3), uint64(42), uint64(99)
	k := OutKey(src, typ, dst, edge)
	gs, gt, gd, ge, err := ParseOutKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if gs != src || gt != typ || gd != dst || ge != edge {
		t.Errorf("got (%d,%d,%d,%d), want (%d,%d,%d,%d)", gs, gt, gd, ge, src, typ, dst, edge)
	}
	if !bytes.HasPrefix(k, OutPrefix(src, typ)) {
		t.Error("OutKey must have OutPrefix as a prefix")
	}
	if !bytes.HasPrefix(k, OutPrefixAll(src)) {
		t.Error("OutKey must have OutPrefixAll as a prefix")
	}
}

func TestInKeyRoundTrip(t *testing.T) {
	dst, typ, src, edge := uint64(7), uint32(3), uint64(42), uint64(99)
	k := InKey(dst, typ, src, edge)
	gd, gt, gs, ge, err := ParseInKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if gd != dst || gt != typ || gs != src || ge != edge {
		t.Errorf("got (%d,%d,%d,%d), want (%d,%d,%d,%d)", gd, gt, gs, ge, dst, typ, src, edge)
	}
	if !bytes.HasPrefix(k, InPrefix(dst, typ)) {
		t.Error("InKey must have InPrefix as a prefix")
	}
}

func TestLabelKeyRoundTrip(t *testing.T) {
	label, node := uint32(5), uint64(123)
	k := LabelKey(label, node)
	gl, gn, err := ParseLabelKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if gl != label || gn != node {
		t.Errorf("got (%d,%d), want (%d,%d)", gl, gn, label, node)
	}
	if !bytes.HasPrefix(k, LabelPrefix(label)) {
		t.Error("LabelKey must have LabelPrefix as a prefix")
	}
}

func TestNodeKeyRoundTrip(t *testing.T) {
	k := NodeKey(0xCAFE)
	id, err := NodeIDFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if id != 0xCAFE {
		t.Errorf("got %d", id)
	}
}

func TestPropKeyOrdering(t *testing.T) {
	const label, propKey = uint32(1), uint32(2)
	// Same value, different nodeID: orders by nodeID.
	a, _ := PropKey(label, propKey, int64(10), 1)
	b, _ := PropKey(label, propKey, int64(10), 2)
	if bytes.Compare(a, b) >= 0 {
		t.Error("with equal value, the smaller nodeID must order first")
	}
	// A different value dominates over the nodeID.
	c, _ := PropKey(label, propKey, int64(5), 999)
	if bytes.Compare(c, a) >= 0 {
		t.Error("a smaller value must order first regardless of the nodeID")
	}
	// All keys share PropPrefix for (label, propKey, value).
	pfx, _ := PropPrefix(label, propKey, int64(10))
	if !bytes.HasPrefix(a, pfx) || !bytes.HasPrefix(b, pfx) {
		t.Error("PropKey must have PropPrefix as a prefix")
	}
	if node, err := ParsePropKeyNode(a); err != nil || node != 1 {
		t.Errorf("ParsePropKeyNode(a) = (%d, %v)", node, err)
	}
}
