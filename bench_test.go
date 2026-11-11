package intreccio

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkQuery measures end-to-end (parse + sema + plan + exec) latency of an
// indexed point lookup over 1000 Person nodes.
func BenchmarkQuery(b *testing.B) {
	const n = 1000
	db, err := OpenInMemory()
	if err != nil {
		b.Fatalf("OpenInMemory: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if _, err := db.Query(ctx, "CREATE INDEX FOR (p:Person) ON (p.email)", nil); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Query(ctx,
			"CREATE (n:Person {email: $e})",
			map[string]any{"e": fmt.Sprintf("p%d@x.com", i)}); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Query(ctx,
			"MATCH (p:Person) WHERE p.email = $e RETURN p",
			map[string]any{"e": fmt.Sprintf("p%d@x.com", i%n)}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLiveQueryUpdate measures what one relevant write costs a live query:
// the write itself plus the re-evaluation that produces the next batch. The
// graph is `pairs` disjoint (a)-[:KNOWS]->(b) pairs; each iteration renames one
// node, which changes exactly one row of the result.
//
// "anchored" re-evaluates around the changed node, so its cost should not
// depend on the size of the result; "full" re-runs the query, so it should grow
// with it.
func BenchmarkLiveQueryUpdate(b *testing.B) {
	for _, pairs := range []int{100, 1000, 10000} {
		for _, mode := range []string{"anchored", "full"} {
			b.Run(fmt.Sprintf("%s/pairs=%d", mode, pairs), func(b *testing.B) {
				old := anchoringEnabled
				anchoringEnabled = mode == "anchored"
				defer func() { anchoringEnabled = old }()

				db, err := OpenInMemory()
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				ctx := context.Background()
				mustB := func(cypher string, params map[string]any) {
					b.Helper()
					if _, err := db.Query(ctx, cypher, params); err != nil {
						b.Fatalf("%s: %v", cypher, err)
					}
				}
				mustB("CREATE INDEX FOR (p:Person) ON (p.id)", nil)
				// Seeded in chunks: one transaction cannot hold the largest graph.
				const chunk = 1000
				for from := 0; from < pairs; from += chunk {
					ids := make([]any, 0, chunk)
					for i := from; i < min(from+chunk, pairs); i++ {
						ids = append(ids, int64(i))
					}
					mustB("UNWIND $ids AS i CREATE (:Person {id: i, name: 'a'})-[:KNOWS]->(:Person {id: -i - 1, name: 'b'})",
						map[string]any{"ids": ids})
				}

				sub, err := db.Subscribe(ctx, "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS a, b.name AS b", nil,
					WithSubscriptionMaxRows(0))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = sub.Close() }()
				if first := <-sub.Changes(); len(first.Added) != pairs {
					b.Fatalf("initial result has %d rows, want %d", len(first.Added), pairs)
				}

				b.ResetTimer()
				for i := range b.N {
					mustB("MATCH (p:Person {id: $id}) SET p.name = $name",
						map[string]any{"id": int64(i % pairs), "name": fmt.Sprintf("n%d", i)})
					if c := <-sub.Changes(); len(c.Added) != 1 || len(c.Removed) != 1 {
						b.Fatalf("batch = +%d -%d, want +1 -1", len(c.Added), len(c.Removed))
					}
				}
				b.StopTimer()
				if mode == "anchored" && db.liveStats.anchored.Load() < int64(b.N) {
					b.Fatalf("only %d of %d evaluations were anchored", db.liveStats.anchored.Load(), b.N)
				}
			})
		}
	}
}
