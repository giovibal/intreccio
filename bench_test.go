package mycypher

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
