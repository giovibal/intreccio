package changefeed

import (
	"reflect"
	"testing"
)

func rows(ss ...string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

func strs(bs [][]byte) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = string(b)
	}
	return out
}

func TestResultSetReplace(t *testing.T) {
	cases := []struct {
		name        string
		prev, cur   []string
		wantAdded   []int
		wantRemoved []string
	}{
		{"initial", nil, []string{"a", "b"}, []int{0, 1}, nil},
		{"unchanged", []string{"a", "b"}, []string{"a", "b"}, nil, nil},
		{"reordered", []string{"a", "b"}, []string{"b", "a"}, nil, nil},
		{"added in result order", []string{"b"}, []string{"a", "b", "c"}, []int{0, 2}, nil},
		{"removed in previous order", []string{"c", "a", "b"}, []string{"a"}, nil, []string{"c", "b"}},
		{"replaced", []string{"a"}, []string{"b"}, []int{0}, []string{"a"}},
		{"emptied", []string{"a", "b"}, nil, nil, []string{"a", "b"}},
		{"duplicate added", []string{"a"}, []string{"a", "a"}, []int{1}, nil},
		{"duplicate removed", []string{"a", "a", "b"}, []string{"a", "c"}, []int{1}, []string{"a", "b"}},
		{"duplicates unchanged", []string{"a", "a"}, []string{"a", "a"}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rs ResultSet
			rs.Replace(rows(c.prev...))
			added, removed := rs.Replace(rows(c.cur...))
			if !reflect.DeepEqual(added, c.wantAdded) {
				t.Errorf("added = %v, want %v", added, c.wantAdded)
			}
			var got []string
			if removed != nil {
				got = strs(removed)
			}
			if !reflect.DeepEqual(got, c.wantRemoved) {
				t.Errorf("removed = %v, want %v", got, c.wantRemoved)
			}
			if rs.Len() != len(c.cur) {
				t.Errorf("Len = %d, want %d", rs.Len(), len(c.cur))
			}
		})
	}
}
