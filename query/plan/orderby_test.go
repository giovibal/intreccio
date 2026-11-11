package plan

import (
	"strings"
	"testing"

	"github.com/giovibal/intreccio/query/parser"
)

// TestSortPlacement covers #75: the Sort goes below the projection when a key
// reads a variable the projection drops, above it otherwise.
func TestSortPlacement(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"projected alias: above",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY name DESC",
			`Sort(name DESC)
  Project(p.name AS name)
    NodeByLabelScan(p:Person)`,
		},
		{
			"non-projected key: below",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC",
			`Project(p.name AS name)
  Sort(p.score DESC)
    NodeByLabelScan(p:Person)`,
		},
		{
			"skip and limit stay above the projection",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score DESC SKIP 1 LIMIT 2",
			`Limit(2)
  Skip(1)
    Project(p.name AS name)
      Sort(p.score DESC)
        NodeByLabelScan(p:Person)`,
		},
		{
			"mixed keys: the alias is inlined",
			"MATCH (p:Person) RETURN p.name AS name, p.score * 2 AS double ORDER BY double DESC, p.age",
			`Project(p.name AS name, p.score * 2 AS double)
  Sort(p.score * 2 DESC, p.age)
    NodeByLabelScan(p:Person)`,
		},
		{
			"a projected variable is kept as is below",
			"MATCH (p:Person)-[r:KNOWS]->(q) RETURN q ORDER BY q.name, r.since",
			`Project(q)
  Sort(q.name, r.since)
    Expand(p)-[r:KNOWS]->(q)
      NodeByLabelScan(p:Person)`,
		},
		{
			"repeated projected expression: read from its column",
			"MATCH (p:Person) RETURN p.name ORDER BY p.name",
			`Sort(p.name)
  Project(p.name)
    NodeByLabelScan(p:Person)`,
		},
		{
			"repeated expression inside a larger key",
			"MATCH (p:Person) RETURN DISTINCT p.age + 1 AS next ORDER BY (p.age + 1) * 2",
			`Sort(next * 2)
  Project(DISTINCT p.age + 1 AS next)
    NodeByLabelScan(p:Person)`,
		},
		{
			"repeated aggregation",
			"MATCH (p:Person) RETURN p.city AS city, count(*) AS n ORDER BY count(*) DESC, p.city",
			`Sort(n DESC, city)
  Aggregate(group=[p.city AS city] aggs=[count(*) AS n])
    NodeByLabelScan(p:Person)`,
		},
		{
			"a column name hides the variable",
			"MATCH (p:Person) RETURN p.name AS p ORDER BY p",
			`Sort(p)
  Project(p.name AS p)
    NodeByLabelScan(p:Person)`,
		},
		{
			"WITH: sort below, WHERE above the projection",
			"MATCH (p:Person) WITH p.name AS name ORDER BY p.score WHERE name <> 'x' RETURN name",
			`Project(name)
  Filter(name <> 'x')
    Project(p.name AS name)
      Sort(p.score)
        NodeByLabelScan(p:Person)`,
		},
		{
			"comprehension variable is not a column reference",
			"MATCH (p:Person) RETURN p.name AS x ORDER BY size([x IN p.tags WHERE x <> p.name])",
			`Project(p.name AS x)
  Sort(size([x IN p.tags WHERE x <> p.name]))
    NodeByLabelScan(p:Person)`,
		},
		{
			"comprehension body is not matched against the projection",
			"MATCH (p:Person) RETURN DISTINCT p.tags AS tags, p.name ORDER BY size([p IN p.tags WHERE p.name = 1])",
			`Sort(size([p IN tags WHERE p.name = 1]))
  Project(DISTINCT p.tags AS tags, p.name)
    NodeByLabelScan(p:Person)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(Explain(mustPlan(t, tc.src, withIndex())))
			if got != tc.want {
				t.Errorf("EXPLAIN %s\n got:\n%s\nwant:\n%s", tc.src, got, tc.want)
			}
		})
	}
}

func TestSortPlacementErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			"non-deterministic column sorted together with a dropped variable",
			"MATCH (p:Person) RETURN rand() AS r, p.name AS name ORDER BY r, p.score",
			`column "r"`,
		},
		{
			"column read where a variable it is computed from is redefined",
			"MATCH (p:Person) RETURN p.name AS name ORDER BY p.score, size([p IN [1] WHERE name = p])",
			`column "name"`,
		},
		{
			// Rejected by the analyzer first; the planner must not rely on it.
			"dropped variable after DISTINCT",
			"MATCH (p:Person) RETURN DISTINCT p.name AS name ORDER BY p.score",
			"DISTINCT or an aggregation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parser.Parse(tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = Plan(q, withIndex())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Plan(%s) error = %v, want it to mention %s", tc.src, err, tc.want)
			}
		})
	}

	// A non-deterministic column sorted on its own is computed once: fine.
	mustPlan(t, "MATCH (p:Person) RETURN rand() AS r ORDER BY r", withIndex())
}
