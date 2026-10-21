package codec

import (
	"bytes"
	"strings"
	"testing"
	"testing/quick"
)

// An indexed string starts with s exactly when its encoding starts with
// AppendStringPrefix(s): range scans for STARTS WITH rely on it.
func TestAppendStringPrefixMatchesStartsWith(t *testing.T) {
	prop := func(s, x string) bool {
		enc, err := AppendIndexValue(nil, x)
		if err != nil {
			return false
		}
		return bytes.HasPrefix(enc, AppendStringPrefix(nil, s)) == strings.HasPrefix(x, s)
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Error(err)
	}
	// Strings built to share a prefix, including NUL bytes.
	for _, c := range []struct{ s, x string }{
		{"a", "abc"}, {"a\x00", "a\x00b"}, {"a\x00", "a"}, {"", "anything"}, {"ab", "a"}, {"a", "a\x00"},
	} {
		if !prop(c.s, c.x) {
			t.Errorf("prefix %q / value %q disagree with strings.HasPrefix", c.s, c.x)
		}
	}
}

func TestParsePropKeyRoundTrip(t *testing.T) {
	for _, v := range []any{nil, true, int64(-7), 3.5, "", "a\x00b"} {
		key, err := PropKey(3, 9, v, 42)
		if err != nil {
			t.Fatal(err)
		}
		got, node, err := ParsePropKey(key)
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		if got != v || node != 42 {
			t.Errorf("ParsePropKey(%v) = %v, %d", v, got, node)
		}
		if !bytes.HasPrefix(key, PropBandPrefix(3, 9, key[9])) {
			t.Errorf("%v: key does not start with its band prefix", v)
		}
	}
	if _, _, err := ParsePropKey([]byte{KeyProp, 0, 0}); err == nil {
		t.Error("truncated key: expected an error")
	}
}
