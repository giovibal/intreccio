package graph

import (
	"math"
	"testing"
)

func TestUniqueValue(t *testing.T) {
	tests := []struct {
		in   any
		want any
		ok   bool
	}{
		{int64(1), int64(1), true},
		{1.0, int64(1), true},
		{math.Copysign(0, -1), int64(0), true},
		{1.5, 1.5, true},
		{math.Inf(1), math.Inf(1), true},
		{9.3e18, 9.3e18, true}, // beyond int64
		{-9.223372036854775808e18, int64(math.MinInt64), true},
		{math.NaN(), nil, false},
		{"1", "1", true},
		{true, true, true},
	}
	for _, tc := range tests {
		got, ok := uniqueValue(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("uniqueValue(%v) = %v (%T), %v; want %v (%T), %v", tc.in, got, got, ok, tc.want, tc.want, tc.ok)
		}
	}
}
