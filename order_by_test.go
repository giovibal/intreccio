package intreccio

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// orderByDB holds four people whose insertion order differs from every order
// the tests sort on, so a sort that silently does nothing is caught.
//
//	name  score  age  city
//	b     2      20   Rome
//	d     4      40   Rome
//	a     1      30   Pisa
//	c     3      20   Pisa
func orderByDB(t *testing.T) *DB {
	t.Helper()
	db := newDB(t)
	mustQuery(t, db, `CREATE
		(:Person {name: 'b', score: 2, age: 20, city: 'Rome', tags: ['x']}),
		(:Person {name: 'd', score: 4, age: 40, city: 'Rome', tags: ['x', 'y', 'z']}),
		(:Person {name: 'a', score: 1, age: 30, city: 'Pisa', tags: []}),
		(:Person {name: 'c', score: 3, age: 20, city: 'Pisa', tags: ['x', 'y']})`, nil)
	return db
}

// rowsText renders rows compactly: [[a 1] [b 2]] -> "a 1, b 2".
func rowsText(rows [][]any) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		for j, c := range r {
			cells[j] = fmt.Sprint(c)
		}
		parts[i] = strings.Join(cells, " ")
	}
	return strings.Join(parts, ", ")
}

// TestOrderBy covers #75: ORDER BY on an expression that is not projected
// used to be planned above the projection, where it silently did not sort.
func TestOrderBy(t *testing.T) {
	db := orderByDB(t)
	cases := []struct {
		name, query, want string
	}{
		{"non-projected key",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC",
			"d, c, b, a"},
		{"non-projected key ascending",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score",
			"a, b, c, d"},
		{"non-projected key with LIMIT",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC LIMIT 2",
			"d, c"},
		{"non-projected key with SKIP and LIMIT",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC SKIP 1 LIMIT 2",
			"c, b"},
		{"non-projected key, unaliased item",
			"MATCH (p:Person) RETURN p.name ORDER BY p.score DESC",
			"d, c, b, a"},
		{"non-projected expression",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.age * 10 + p.score DESC",
			"d, a, c, b"},
		{"projected alias",
			"MATCH (p:Person) RETURN p.name AS name, p.score AS score ORDER BY score DESC LIMIT 2",
			"d 4, c 3"},
		{"projected alias only, key expression on it",
			"MATCH (p:Person) RETURN p.name AS name, p.score AS score ORDER BY -score",
			"d 4, c 3, b 2, a 1"},
		{"mixed: alias then non-projected",
			"MATCH (p:Person) RETURN p.name AS name, p.age AS age ORDER BY age, p.score DESC",
			"c 20, b 20, a 30, d 40"},
		{"mixed: non-projected then alias",
			"MATCH (p:Person) RETURN p.name AS name, p.score AS score ORDER BY p.city, score DESC",
			"c 3, a 1, d 4, b 2"},
		{"mixed: computed alias",
			"MATCH (p:Person) RETURN p.name AS name, p.age * 2 AS twice ORDER BY twice DESC, p.score",
			"d 80, a 60, b 40, c 40"},
		{"repeated projected expression",
			"MATCH (p:Person) RETURN p.score + 1 ORDER BY p.score + 1 DESC",
			"5, 4, 3, 2"},
		{"a column name hides the variable",
			"MATCH (p:Person) RETURN p.name AS p ORDER BY p DESC",
			"d, c, b, a"},
		{"RETURN *",
			"MATCH (p:Person) WITH p.name AS name, p.score AS score RETURN * ORDER BY score DESC",
			"d 4, c 3, b 2, a 1"},
		{"key through a relationship variable that is not returned",
			"MATCH (p:Person) UNWIND p.tags AS tag RETURN tag ORDER BY p.score DESC, tag DESC LIMIT 4",
			"z, y, x, y"},
		{"comprehension in the key",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY size([t IN p.tags WHERE t <> 'x']) DESC, p.name",
			"d, c, a, b"},

		{"WITH: non-projected key",
			"MATCH (p:Person) WITH p.name AS name ORDER BY p.score DESC RETURN name",
			"d, c, b, a"},
		{"WITH: non-projected key and LIMIT",
			"MATCH (p:Person) WITH p.name AS name ORDER BY p.score DESC LIMIT 2 RETURN name",
			"d, c"},
		{"WITH: mixed keys",
			"MATCH (p:Person) WITH p.name AS name, p.age AS age ORDER BY age DESC, p.score DESC SKIP 1 RETURN name, age",
			"a 30, c 20, b 20"},
		{"WITH: projected alias",
			"MATCH (p:Person) WITH p.name AS name, p.score AS score ORDER BY score DESC LIMIT 3 RETURN name",
			"d, c, b"},
		{"WITH then RETURN, each with its own non-projected key",
			"MATCH (p:Person) WITH p, p.name AS name ORDER BY p.score DESC LIMIT 3 RETURN name ORDER BY p.age, p.score",
			"b, c, d"},

		{"DISTINCT: projected alias",
			"MATCH (p:Person) RETURN DISTINCT p.age AS age ORDER BY age DESC",
			"40, 30, 20"},
		{"DISTINCT: repeated projected expression",
			"MATCH (p:Person) RETURN DISTINCT p.age ORDER BY p.age DESC",
			"40, 30, 20"},
		{"DISTINCT: repeated expression inside a larger key",
			"MATCH (p:Person) RETURN DISTINCT p.age AS age ORDER BY -p.age",
			"40, 30, 20"},
		{"aggregation: alias",
			"MATCH (p:Person) RETURN p.city AS city, sum(p.score) AS total ORDER BY total DESC",
			"Rome 6, Pisa 4"},
		{"aggregation: repeated aggregate and grouping key",
			"MATCH (p:Person) RETURN p.age AS age, count(*) AS n ORDER BY count(*) DESC, p.age DESC",
			"20 2, 40 1, 30 1"},
		{"aggregation: grouped node is still a column",
			"MATCH (p:Person) RETURN p, count(*) AS n ORDER BY p.score DESC LIMIT 1",
			""},
		{"WITH aggregation: repeated aggregate",
			"MATCH (p:Person) WITH p.city AS city, count(*) AS n ORDER BY max(p.score), n RETURN city",
			""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := db.Query(context.Background(), tc.query, nil)
			switch tc.name {
			case "aggregation: grouped node is still a column":
				if err != nil {
					t.Fatalf("Query: %v", err)
				}
				if len(res.Rows) != 1 || res.Rows[0][0].(Node).Props["name"] != "d" {
					t.Errorf("rows = %v, want the node named d", res.Rows)
				}
				return
			case "WITH aggregation: repeated aggregate":
				// max(p.score) is not projected: a clear error, not a no-op.
				if err == nil || !strings.Contains(err.Error(), "ORDER BY cannot compute max") {
					t.Errorf("Query error = %v, want the aggregation rejected", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := rowsText(res.Rows); got != tc.want {
				t.Errorf("%s\n got: %s\nwant: %s", tc.query, got, tc.want)
			}
		})
	}
}

// TestOrderByRejected covers the sort keys that cannot be honoured: they are
// errors, never an unsorted result.
func TestOrderByRejected(t *testing.T) {
	db := orderByDB(t)
	cases := []struct {
		name, query, want string
	}{
		{"DISTINCT and a variable that is not returned",
			"MATCH (p:Person) RETURN DISTINCT p.city AS city ORDER BY p.score",
			"ORDER BY cannot use variable p"},
		{"aggregation and a variable that is not returned",
			"MATCH (p:Person) RETURN p.city AS city, count(*) AS n ORDER BY p.score",
			"ORDER BY cannot use variable p"},
		{"WITH DISTINCT and a variable that is not projected",
			"MATCH (p:Person) WITH DISTINCT p.city AS city ORDER BY p.score RETURN city",
			"ORDER BY cannot use variable p"},
		{"aggregation that is not returned",
			"MATCH (p:Person) RETURN p.city AS city, count(*) AS n ORDER BY sum(p.score)",
			"ORDER BY cannot compute sum"},
		{"aggregation in a plain projection",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY count(*)",
			"ORDER BY cannot compute count"},
		{"non-deterministic column with a variable that is not returned",
			"MATCH (p:Person) RETURN rand() AS r, p.name AS name ORDER BY r, p.score",
			`column "r"`},
		{"a key that fails to evaluate",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY range(1, p.name)",
			"range() expects integers"},
		{"a key that fails to evaluate above the projection",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY range(1, name)",
			"range() expects integers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := db.Query(context.Background(), tc.query, nil)
			if err == nil {
				t.Fatalf("Query(%s) = %v, want an error", tc.query, res.Rows)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A non-deterministic column sorted on its own is computed once, so the rows
// come back in the order of the values returned.
func TestOrderByRandColumn(t *testing.T) {
	db := orderByDB(t)
	res := mustQuery(t, db, "MATCH (p:Person) RETURN rand() AS r, p.name AS name ORDER BY r", nil)
	if len(res.Rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(res.Rows))
	}
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i-1][0].(float64) > res.Rows[i][0].(float64) {
			t.Fatalf("rows are not sorted by r: %v", res.Rows)
		}
	}
}

func TestOrderByExplain(t *testing.T) {
	db := orderByDB(t)
	cases := []struct{ query, want string }{
		{"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC LIMIT 2",
			`Limit(2)
  Project(p.name AS name)
    Sort(p.score DESC)
      NodeByLabelScan(p:Person)`},
		{"MATCH (p:Person) RETURN p.name AS name, p.score AS score ORDER BY score DESC LIMIT 2",
			`Limit(2)
  Sort(score DESC)
    Project(p.name AS name, p.score AS score)
      NodeByLabelScan(p:Person)`},
		{"MATCH (p:Person) WITH p.name AS name ORDER BY p.score DESC RETURN name",
			`Project(name)
  Project(p.name AS name)
    Sort(p.score DESC)
      NodeByLabelScan(p:Person)`},
	}
	for _, tc := range cases {
		got, err := db.Explain(context.Background(), tc.query)
		if err != nil {
			t.Fatalf("Explain(%s): %v", tc.query, err)
		}
		if strings.TrimSpace(got) != tc.want {
			t.Errorf("Explain(%s)\n got:\n%s\nwant:\n%s", tc.query, got, tc.want)
		}
	}
}

// A live query with ORDER BY ... LIMIT on a non-projected key follows the
// right top-N.
func TestSubscribeOrderByNonProjectedKey(t *testing.T) {
	db := orderByDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := db.Subscribe(ctx, "MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC LIMIT 2", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	next := func() Change {
		t.Helper()
		select {
		case c, ok := <-sub.Changes():
			if !ok {
				t.Fatalf("subscription closed: %v", sub.Err())
			}
			return c
		case <-ctx.Done():
			t.Fatal("timed out waiting for a change")
		}
		return Change{}
	}

	if c := next(); !c.Initial || rowsText(c.Added) != "d, c" {
		t.Fatalf("initial batch = %+v, want d, c", c)
	}
	mustQuery(t, db, "CREATE (:Person {name: 'e', score: 10})", nil)
	if c := next(); rowsText(c.Added) != "e" || rowsText(c.Removed) != "c" {
		t.Fatalf("change = %+v, want e added and c removed", c)
	}
}
