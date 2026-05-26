package parser

import "testing"

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
