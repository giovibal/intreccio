package intreccio

import (
	"context"
	"errors"
	"math"
	"testing"
)

// TestIntegerOverflowIsAnError covers #23: integer arithmetic that leaves the
// int64 range fails instead of wrapping around.
func TestIntegerOverflowIsAnError(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	params := map[string]any{"max": int64(math.MaxInt64), "min": int64(math.MinInt64)}

	overflows := []string{
		"$max + 1",
		"1 + $max",
		"$min - 1",
		"0 - $min",
		"$max * 2",
		"$min * -1",
		"-1 * $min",
		"$min / -1",
		"-$min",
		"abs($min)",
	}
	for _, expr := range overflows {
		_, err := db.Query(ctx, "RETURN "+expr+" AS v", params)
		if !errors.Is(err, ErrIntegerOverflow) {
			t.Errorf("%s: got err %v, want ErrIntegerOverflow", expr, err)
		}
	}

	ok := []struct {
		expr string
		want any
	}{
		{"$max - 1", int64(math.MaxInt64 - 1)},
		{"$min + 1", int64(math.MinInt64 + 1)},
		{"$max * 1", int64(math.MaxInt64)},
		{"$min * 1", int64(math.MinInt64)},
		{"$max / -1", int64(-math.MaxInt64)},
		{"$min % -1", int64(0)},
		{"-$max", int64(-math.MaxInt64)},
		{"3037000499 * 3037000499", int64(9223372030926249001)},
		{"$max + 1.0", float64(math.MaxInt64) + 1}, // float arithmetic is unaffected
	}
	for _, c := range ok {
		res, err := db.Query(ctx, "RETURN "+c.expr+" AS v", params)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := res.Rows[0][0]; got != c.want {
			t.Errorf("%s = %v, want %v", c.expr, got, c.want)
		}
	}

	if _, err := db.Query(ctx, "UNWIND [$max, 1] AS x RETURN sum(x)", params); !errors.Is(err, ErrIntegerOverflow) {
		t.Errorf("sum overflow: got err %v, want ErrIntegerOverflow", err)
	}
	res, err := db.Query(ctx, "UNWIND [$max, 1, -2] AS x RETURN sum(x)", params)
	_ = res
	if !errors.Is(err, ErrIntegerOverflow) {
		// The running sum leaves the range even though the total fits:
		// openCypher (and Neo4j) report the overflow.
		t.Errorf("sum with intermediate overflow: got err %v, want ErrIntegerOverflow", err)
	}
	res, err = db.Query(ctx, "UNWIND [$max, -1, 1] AS x RETURN sum(x)", params)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0]; got != int64(math.MaxInt64) {
		t.Errorf("sum = %v, want MaxInt64", got)
	}
}
