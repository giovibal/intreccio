package sema

import (
	"reflect"
	"strings"
	"testing"

	"github.com/giovibal/intreccio/query/parser"
)

func analyze(t *testing.T, src string) (*Result, error) {
	t.Helper()
	q, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse(%q): %v", src, err)
	}
	return Analyze(q)
}

func TestValidQueries(t *testing.T) {
	cases := []struct {
		src  string
		cols []string
	}{
		{"MATCH (n:Person) RETURN n", []string{"n"}},
		{"MATCH (a)-[:KNOWS]->(b) RETURN a, b", []string{"a", "b"}},
		{"MATCH (p) WITH p AS q RETURN q", []string{"q"}},
		{"MATCH (p)-[:KNOWS]->(f) WITH p, count(*) AS c WHERE c > 1 RETURN p, c", []string{"p", "c"}},
		{"MATCH (a) RETURN a.name AS name ORDER BY name", []string{"name"}},
		{"MATCH (a) RETURN a.name ORDER BY a.name", []string{"a.name"}},
		{"RETURN 1 AS one", []string{"one"}},
		{"MATCH (a) WITH * RETURN a", []string{"a"}},
		{"MATCH (a), (b) RETURN *", []string{"a", "b"}},
		{"MATCH (a)-[r:KNOWS]->(b) WITH b, r RETURN b", []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			res, err := analyze(t, tc.src)
			if err != nil {
				t.Fatalf("Analyze(%q): %v", tc.src, err)
			}
			if !reflect.DeepEqual(res.Columns, tc.cols) {
				t.Errorf("columns = %v, want %v", res.Columns, tc.cols)
			}
		})
	}
}

func TestValidNoColumns(t *testing.T) {
	for _, src := range []string{
		"CREATE (n:Person {name: 'Bob'})",
		"MATCH (n:Person) SET n.age = 1",
		"MATCH (n:Person) SET n:Admin",
		"MATCH (n) SET n = {name: 'x'}",
		"MATCH (n) SET n += {age: 1}",
		"MATCH (n:Person) REMOVE n.email, n:Admin",
		"MATCH (n) DETACH DELETE n",
		"MERGE (n:Person {email: $e})",
		"MERGE (n:Person {email: $e}) ON CREATE SET n.c = 1 ON MATCH SET n.s = 1",
		"CREATE INDEX FOR (p:Person) ON (p.email)",
		"CREATE (a:Person)-[:KNOWS]->(b:Person)",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := analyze(t, src); err != nil {
				t.Errorf("Analyze(%q): %v", src, err)
			}
		})
	}
}

func TestValidExtendedQueries(t *testing.T) {
	cases := []struct {
		src  string
		cols []string
	}{
		{"UNWIND [1,2,3] AS x RETURN x", []string{"x"}},
		{"MATCH (a) UNWIND [1,2] AS i RETURN a, i", []string{"a", "i"}},
		{"MATCH (a) OPTIONAL MATCH (a)-[:T]->(b) RETURN a, b", []string{"a", "b"}},
		// OPTIONAL MATCH keeps b in scope for subsequent clauses.
		{"MATCH (a) OPTIONAL MATCH (a)-[:T]->(b) RETURN a.name AS n, b.name AS m", []string{"n", "m"}},
		// UNION with matching column names is valid.
		{"MATCH (p:Person) RETURN p.name AS n UNION MATCH (c:Company) RETURN c.name AS n", []string{"n"}},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			res, err := analyze(t, tc.src)
			if err != nil {
				t.Fatalf("Analyze(%q): %v", tc.src, err)
			}
			if !reflect.DeepEqual(res.Columns, tc.cols) {
				t.Errorf("columns = %v, want %v", res.Columns, tc.cols)
			}
		})
	}
}

func TestUnionColumnMismatch(t *testing.T) {
	for _, src := range []string{
		"MATCH (a) RETURN a UNION MATCH (b) RETURN b.name",
		"MATCH (a) RETURN a AS x UNION MATCH (b) RETURN b AS y",
		"MATCH (a) RETURN a, count(*) AS c UNION MATCH (b) RETURN b",
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := analyze(t, src); err == nil {
				t.Errorf("Analyze(%q): expected an error for mismatched UNION columns", src)
			} else if _, ok := err.(*SemaError); !ok {
				t.Errorf("expected *SemaError, got %T: %v", err, err)
			}
		})
	}
}

func TestUndefinedVariable(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		col  int // 0 = not checked
	}{
		{"return", "MATCH (n) RETURN m", 1, 18},
		{"dropped-by-with", "MATCH (a)-[]->(b) WITH a RETURN b", 1, 0},
		{"where", "MATCH (a) WHERE b.x = 1 RETURN a", 1, 0},
		{"set-target", "MATCH (a) SET b.x = 1", 1, 0},
		{"order-by", "MATCH (a) RETURN a AS x ORDER BY y", 1, 0},
		{"set-label-undefined", "SET m:Foo", 1, 0},
		{"set-map-undefined", "SET m = {a: 1}", 1, 0},
		{"set-map-merge-undefined", "SET m += {a: 1}", 1, 0},
		{"remove-property-undefined", "REMOVE m.x", 1, 0},
		{"remove-label-undefined", "REMOVE m:L", 1, 0},
		{"unwind-from-undefined", "WITH 1 AS x UNWIND y AS z RETURN z", 1, 0},
		{"merge-on-create-undefined", "MERGE (n:P {x: 1}) ON CREATE SET m.y = 1", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := analyze(t, tc.src)
			se := wantSemaError(t, err)
			if se.Pos.Line != tc.line {
				t.Errorf("line = %d, want %d (%v)", se.Pos.Line, tc.line, se)
			}
			if tc.col != 0 && se.Pos.Col != tc.col {
				t.Errorf("column = %d, want %d (%v)", se.Pos.Col, tc.col, se)
			}
		})
	}
}

func TestOtherSemanticErrors(t *testing.T) {
	for _, src := range []string{
		"MATCH (p) WITH p.name RETURN p",                    // WITH without alias
		"RETURN *",                                          // RETURN * with no scope
		"MATCH (a) RETURN count(sum(a.x))",                  // nested aggregation
		"CREATE INDEX FOR (p:Person) ON (p.email) RETURN p", // CREATE INDEX not standalone
		"MATCH (n)",                                         // incomplete query
		"MATCH (a) WHERE a.x = 1",                           // incomplete (missing RETURN)
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := analyze(t, src); err == nil {
				t.Errorf("Analyze(%q): expected an error", src)
			} else if _, ok := err.(*SemaError); !ok {
				t.Errorf("expected *SemaError, got %T: %v", err, err)
			}
		})
	}
}

func TestReturnNotLast(t *testing.T) {
	// RETURN followed by more clauses.
	_, err := analyze(t, "MATCH (n) RETURN n WITH n AS m RETURN m")
	se := wantSemaError(t, err)
	if se.Pos.Line != 1 {
		t.Errorf("line = %d (%v)", se.Pos.Line, se)
	}
}

func wantSemaError(t *testing.T, err error) *SemaError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	se, ok := err.(*SemaError)
	if !ok {
		t.Fatalf("expected *SemaError, got %T: %v", err, err)
	}
	return se
}

// TestStreamMustBeReadOnly checks that a STREAM (live) query is accepted when
// it only reads and rejected as soon as any arm writes.
func TestStreamMustBeReadOnly(t *testing.T) {
	for _, src := range []string{
		"STREAM MATCH (n:Person) RETURN n.name AS name",
		"STREAM MATCH (n) WITH n.x AS x WHERE x > 1 RETURN x ORDER BY x LIMIT 3",
		"STREAM MATCH (n) RETURN count(n) AS c",
		"STREAM UNWIND [1, 2] AS x RETURN x",
		"STREAM MATCH (a) RETURN a.x AS x UNION MATCH (b) RETURN b.x AS x",
	} {
		if _, err := analyze(t, src); err != nil {
			t.Errorf("Analyze(%q): %v", src, err)
		}
	}
	for _, src := range []string{
		"STREAM CREATE (n:Person)",
		"STREAM CREATE (n:Person) RETURN n",
		"STREAM MATCH (n) SET n.x = 1 RETURN n",
		"STREAM MATCH (n) REMOVE n.x RETURN n",
		"STREAM MATCH (n) DETACH DELETE n",
		"STREAM MERGE (n:Person {name: 'a'}) RETURN n",
		"STREAM CREATE INDEX FOR (p:Person) ON (p.email)",
	} {
		_, err := analyze(t, src)
		se := wantSemaError(t, err)
		if se != nil && !strings.Contains(se.Msg, "STREAM") {
			t.Errorf("Analyze(%q): error %q does not mention STREAM", src, se.Msg)
		}
	}
}

// TestOrderByScope covers #75: what the sort keys of a RETURN / WITH may read.
func TestOrderByScope(t *testing.T) {
	valid := []string{
		// A plain projection: the variables before it are still readable.
		"MATCH (p) RETURN p.name AS name ORDER BY p.score",
		"MATCH (p) RETURN p.name AS name ORDER BY name, p.score DESC",
		"MATCH (p) WITH p.name AS name ORDER BY p.score RETURN name",
		"MATCH (p) WITH p.name AS name ORDER BY p.score LIMIT 2 RETURN name",
		"MATCH (p)-[r]->(q) RETURN q.name AS name ORDER BY r.since, p.name",
		"MATCH (p) RETURN * ORDER BY p.score",
		// DISTINCT / aggregation: projected columns and expressions only.
		"MATCH (p) RETURN DISTINCT p.age AS age ORDER BY age",
		"MATCH (p) RETURN DISTINCT p.age ORDER BY p.age",
		"MATCH (p) RETURN DISTINCT p ORDER BY p.age",
		"MATCH (p) RETURN DISTINCT p.age + 1 AS next ORDER BY p.age + 1",
		"MATCH (p) RETURN p.city AS city, count(*) AS n ORDER BY n DESC, city",
		"MATCH (p) RETURN p.city AS city, count(*) AS n ORDER BY count(*) DESC, p.city",
		"MATCH (p) RETURN p.city, count(*) ORDER BY count(*) + 1",
		"MATCH (p) RETURN p, count(*) AS n ORDER BY p.name",
		"MATCH (p) WITH p.city AS city, count(*) AS n ORDER BY count(*) RETURN city",
		"MATCH (p) WITH DISTINCT p.city AS city ORDER BY p.city RETURN city",
	}
	for _, src := range valid {
		t.Run(src, func(t *testing.T) {
			if _, err := analyze(t, src); err != nil {
				t.Errorf("Analyze(%q): %v", src, err)
			}
		})
	}

	invalid := []struct {
		src  string
		want string // substring of the message
		col  int    // 0 = not checked
	}{
		{"MATCH (p) RETURN DISTINCT p.name AS name ORDER BY p.score", "ORDER BY cannot use variable p", 51},
		{"MATCH (p) RETURN DISTINCT p.name ORDER BY p.score", "ORDER BY cannot use variable p", 0},
		{"MATCH (p) RETURN p.city AS city, count(*) AS n ORDER BY p.score", "ORDER BY cannot use variable p", 0},
		{"MATCH (p) RETURN count(*) AS n ORDER BY p.score", "ORDER BY cannot use variable p", 0},
		{"MATCH (p) WITH DISTINCT p.name AS name ORDER BY p.score RETURN name", "ORDER BY cannot use variable p", 0},
		{"MATCH (p) WITH p.city AS city, count(*) AS n ORDER BY p.score RETURN city", "ORDER BY cannot use variable p", 0},
		// An aggregation that is not projected cannot be a sort key.
		{"MATCH (p) RETURN p.city AS city, count(*) AS n ORDER BY sum(p.score)", "ORDER BY cannot compute sum", 0},
		{"MATCH (p) RETURN p.name AS name ORDER BY count(*)", "ORDER BY cannot compute count", 0},
		// Still plain undefined variables.
		{"MATCH (p) RETURN p.name AS name ORDER BY q.score", "undefined variable: q", 0},
		{"MATCH (p) RETURN DISTINCT p.name AS name ORDER BY q.score", "undefined variable: q", 0},
		// WHERE of a WITH still sees the projected columns only.
		{"MATCH (p) WITH p.name AS name WHERE p.score > 1 RETURN name", "undefined variable: p", 0},
		// A variable dropped by WITH stays dropped for later clauses.
		{"MATCH (p) WITH p.name AS name ORDER BY p.score RETURN p", "undefined variable: p", 0},
	}
	for _, tc := range invalid {
		t.Run(tc.src, func(t *testing.T) {
			_, err := analyze(t, tc.src)
			se, ok := err.(*SemaError)
			if !ok {
				t.Fatalf("Analyze(%q) = %v, want a *SemaError", tc.src, err)
			}
			if !strings.Contains(se.Msg, tc.want) {
				t.Errorf("message = %q, want it to contain %q", se.Msg, tc.want)
			}
			if tc.col != 0 && se.Pos.Col != tc.col {
				t.Errorf("column = %d, want %d", se.Pos.Col, tc.col)
			}
		})
	}
}
