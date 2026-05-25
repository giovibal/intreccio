package sema

import (
	"reflect"
	"testing"

	"github.com/giovibal/mycypher/internal/cypher/parser"
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
				t.Errorf("colonne = %v, want %v", res.Columns, tc.cols)
			}
		})
	}
}

func TestValidNoColumns(t *testing.T) {
	for _, src := range []string{
		"CREATE (n:Person {name: 'Bob'})",
		"MATCH (n:Person) SET n.age = 1",
		"MATCH (n) DETACH DELETE n",
		"MERGE (n:Person {email: $e})",
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

func TestUndefinedVariable(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		col  int // 0 = non verificato
	}{
		{"return", "MATCH (n) RETURN m", 1, 18},
		{"dropped-by-with", "MATCH (a)-[]->(b) WITH a RETURN b", 1, 0},
		{"where", "MATCH (a) WHERE b.x = 1 RETURN a", 1, 0},
		{"set-target", "MATCH (a) SET b.x = 1", 1, 0},
		{"order-by", "MATCH (a) RETURN a AS x ORDER BY y", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := analyze(t, tc.src)
			se := wantSemaError(t, err)
			if se.Pos.Line != tc.line {
				t.Errorf("riga = %d, want %d (%v)", se.Pos.Line, tc.line, se)
			}
			if tc.col != 0 && se.Pos.Col != tc.col {
				t.Errorf("colonna = %d, want %d (%v)", se.Pos.Col, tc.col, se)
			}
		})
	}
}

func TestOtherSemanticErrors(t *testing.T) {
	for _, src := range []string{
		"MATCH (p) WITH p.name RETURN p",                    // WITH senza alias
		"RETURN *",                                          // RETURN * senza scope
		"MATCH (a) RETURN count(sum(a.x))",                  // aggregazione annidata
		"CREATE INDEX FOR (p:Person) ON (p.email) RETURN p", // CREATE INDEX non da solo
		"MATCH (n)",                                         // query incompleta
		"MATCH (a) WHERE a.x = 1",                           // incompleta (manca RETURN)
	} {
		t.Run(src, func(t *testing.T) {
			if _, err := analyze(t, src); err == nil {
				t.Errorf("Analyze(%q): atteso errore", src)
			} else if _, ok := err.(*SemaError); !ok {
				t.Errorf("atteso *SemaError, got %T: %v", err, err)
			}
		})
	}
}

func TestReturnNotLast(t *testing.T) {
	// RETURN seguito da altre clausole.
	_, err := analyze(t, "MATCH (n) RETURN n WITH n AS m RETURN m")
	se := wantSemaError(t, err)
	if se.Pos.Line != 1 {
		t.Errorf("riga = %d (%v)", se.Pos.Line, se)
	}
}

func wantSemaError(t *testing.T, err error) *SemaError {
	t.Helper()
	if err == nil {
		t.Fatal("atteso errore, nessuno")
	}
	se, ok := err.(*SemaError)
	if !ok {
		t.Fatalf("atteso *SemaError, got %T: %v", err, err)
	}
	return se
}
