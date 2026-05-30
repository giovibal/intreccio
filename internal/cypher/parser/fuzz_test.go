package parser

import (
	"testing"

	"github.com/giovibal/intreccio/internal/cypher/plan"
	"github.com/giovibal/intreccio/internal/cypher/sema"
)

// noIndexCatalog satisfies plan.Catalog for fuzzing without touching storage.
type noIndexCatalog struct{}

func (noIndexCatalog) HasIndex(string, string) bool { return false }

// FuzzParse asserts that Parse never panics, whatever the input looks like. The
// seed corpus covers the main MVP shapes.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"MATCH (n:Person) RETURN n",
		"MATCH (p:Person {email: $e})-[:KNOWS]->(f) RETURN f.name AS name ORDER BY name LIMIT 10",
		"CREATE (n:Person {name: 'Bob', age: 42})",
		"MATCH (a)-[:KNOWS*1..3]->(b) RETURN b",
		"MATCH (n:Person) SET n.age = n.age + 1",
		"MATCH (n) DETACH DELETE n",
		"MERGE (n:Person {email: $e})",
		"WITH 1 AS one RETURN one",
		"RETURN count(DISTINCT p)",
		"CREATE INDEX FOR (p:Person) ON (p.email)",
		"",
		"x",
		"(((",
		"MATCH",
		"RETURN '\\u0000\\n'",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		_, _ = Parse(src)
	})
}

// FuzzPipeline runs Parse → Sema → Plan and asserts that none of these
// stages ever panic on arbitrary input. Errors are expected and acceptable;
// only a panic counts as a failure.
func FuzzPipeline(f *testing.F) {
	seeds := []string{
		"MATCH (n:Person) RETURN n",
		"MATCH (p)-[:KNOWS]->(f) RETURN f.name AS name ORDER BY name SKIP 1 LIMIT 5",
		"MATCH (p:Person {email: $e}) RETURN count(DISTINCT p) AS c",
		"MATCH (a)-[:KNOWS*1..3]->(b) RETURN b",
		"CREATE (n:Person {name: 'Bob'})",
		"MERGE (n:Person {email: $e}) ON CREATE SET n.created = 1 ON MATCH SET n.seen = 1",
		"MATCH (n:P) REMOVE n.email, n:Admin",
		"UNWIND [1, 2, 3] AS x RETURN x",
		"MATCH (a) RETURN a.name AS n UNION MATCH (b) RETURN b.name AS n",
		"MATCH (a) RETURN a.name AS n UNION ALL MATCH (b) RETURN b.name AS n",
		"MATCH (a) OPTIONAL MATCH (a)-[:T]->(b) RETURN a, b",
		"MATCH (a) WHERE a.name STARTS WITH 'A' AND a.email ENDS WITH '@x' RETURN a",
		"MATCH (a) WHERE a.tag IN ['x','y'] RETURN CASE WHEN a.x IS NULL THEN 0 ELSE 1 END AS k",
		"CREATE INDEX FOR (p:Person) ON (p.email)",
		"",
		"x",
		"((",
		"MATCH",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		q, err := Parse(src)
		if err != nil {
			return
		}
		if _, err := sema.Analyze(q); err != nil {
			return
		}
		_, _ = plan.Plan(q, noIndexCatalog{})
	})
}
