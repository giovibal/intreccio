package mycypher_test

import (
	"context"
	"fmt"

	"github.com/giovibal/mycypher"
)

func ExampleDB_Query_createAndMatch() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (n:Person {name: 'Alice', age: 30})", nil)
	res, _ := db.Query(ctx, "MATCH (p:Person) RETURN p.name AS name, p.age AS age", nil)

	fmt.Println(res.Rows[0][0], res.Rows[0][1])
	// Output: Alice 30
}

func ExampleDB_Query_parameters() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (n:Person {email: 'a@x.com', name: 'Alice'})", nil)
	res, _ := db.Query(ctx,
		"MATCH (p:Person {email: $e}) RETURN p.name AS name",
		map[string]any{"e": "a@x.com"})

	fmt.Println(res.Rows[0][0])
	// Output: Alice
}

func ExampleDB_Query_relationships() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx,
		"CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})", nil)
	res, _ := db.Query(ctx,
		"MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS a, b.name AS b", nil)

	fmt.Println(res.Rows[0][0], "->", res.Rows[0][1])
	// Output: Alice -> Bob
}

func ExampleDB_Query_optionalMatch() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(:Person {name: 'Bob'})", nil)
	_, _ = db.Query(ctx, "CREATE (:Person {name: 'Carol'})", nil)
	res, _ := db.Query(ctx,
		"MATCH (a:Person) OPTIONAL MATCH (a)-[:KNOWS]->(b) RETURN a.name AS an, b.name AS bn ORDER BY an", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0], r[1])
	}
	// Output:
	// Alice Bob
	// Bob <nil>
	// Carol <nil>
}

func ExampleDB_Query_variableLengthPath() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// Chain a -> b -> c.
	_, _ = db.Query(ctx,
		"CREATE (a:P {name:'a'})-[:T]->(b:P {name:'b'})-[:T]->(c:P {name:'c'})", nil)
	res, _ := db.Query(ctx,
		"MATCH (s:P {name: 'a'})-[:T*1..2]->(e) RETURN e.name AS n ORDER BY n", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output:
	// b
	// c
}

func ExampleDB_Query_stringPredicates() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, name := range []string{"alice", "amber", "barbara"} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $n})", map[string]any{"n": name})
	}
	res, _ := db.Query(ctx,
		"MATCH (p:P) WHERE p.name STARTS WITH 'a' AND p.name CONTAINS 'm' RETURN p.name AS n ORDER BY n", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output: amber
}

func ExampleDB_Query_inList() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, name := range []string{"Alice", "Bob", "Carol"} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $n})", map[string]any{"n": name})
	}
	res, _ := db.Query(ctx,
		"MATCH (p:P) WHERE p.name IN ['Alice', 'Carol'] RETURN p.name AS n ORDER BY n", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output:
	// Alice
	// Carol
}

func ExampleDB_Query_isNull() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (:P {name: 'Alice', email: 'a@x'})", nil)
	_, _ = db.Query(ctx, "CREATE (:P {name: 'Bob'})", nil)
	res, _ := db.Query(ctx,
		"MATCH (p:P) WHERE p.email IS NULL RETURN p.name AS n", nil)

	fmt.Println(res.Rows[0][0])
	// Output: Bob
}

func ExampleDB_Query_caseExpression() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, p := range []map[string]any{
		{"name": "Alice", "age": int64(17)},
		{"name": "Bob", "age": int64(42)},
	} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $name, age: $age})", p)
	}
	res, _ := db.Query(ctx,
		`MATCH (p:P)
		 RETURN p.name AS n, CASE WHEN p.age >= 18 THEN 'adult' ELSE 'minor' END AS kind
		 ORDER BY n`, nil)

	for _, r := range res.Rows {
		fmt.Println(r[0], r[1])
	}
	// Output:
	// Alice minor
	// Bob adult
}

func ExampleDB_Query_orderByLimitSkip() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, n := range []string{"Dora", "Alice", "Carol", "Bob"} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $n})", map[string]any{"n": n})
	}
	res, _ := db.Query(ctx,
		"MATCH (p:P) RETURN p.name AS n ORDER BY n SKIP 1 LIMIT 2", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output:
	// Bob
	// Carol
}

func ExampleDB_Query_distinct() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, c := range []string{"IT", "IT", "FR", "DE", "FR"} {
		_, _ = db.Query(ctx, "CREATE (:P {country: $c})", map[string]any{"c": c})
	}
	res, _ := db.Query(ctx,
		"MATCH (p:P) RETURN DISTINCT p.country AS c ORDER BY c", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output:
	// DE
	// FR
	// IT
}

func ExampleDB_Query_withChaining() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, p := range []map[string]any{
		{"name": "Alice", "score": int64(80)},
		{"name": "Bob", "score": int64(40)},
		{"name": "Carol", "score": int64(95)},
	} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $name, score: $score})", p)
	}
	res, _ := db.Query(ctx,
		`MATCH (p:P) WITH p WHERE p.score >= 50
		 RETURN p.name AS n ORDER BY n`, nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	// Output:
	// Alice
	// Carol
}

func ExampleDB_Query_unwind() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	res, _ := db.Query(context.Background(),
		"UNWIND [10, 20, 30] AS x RETURN x", nil)

	for _, r := range res.Rows {
		fmt.Println(r[0])
	}
	_ = ctx
	// Output:
	// 10
	// 20
	// 30
}

func ExampleDB_Query_aggregations() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, s := range []int64{10, 20, 30} {
		_, _ = db.Query(ctx, "CREATE (:P {score: $s})", map[string]any{"s": s})
	}
	res, _ := db.Query(ctx,
		`MATCH (p:P)
		 RETURN count(p) AS c, sum(p.score) AS s, avg(p.score) AS a,
		        min(p.score) AS mn, max(p.score) AS mx`, nil)

	r := res.Rows[0]
	fmt.Println(r[0], r[1], r[2], r[3], r[4])
	// Output: 3 60 20 10 30
}

func ExampleDB_Query_collect() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	for _, n := range []string{"Alice", "Bob", "Bob"} {
		_, _ = db.Query(ctx, "CREATE (:P {name: $n})", map[string]any{"n": n})
	}
	res, _ := db.Query(ctx,
		`MATCH (p:P)
		 WITH p.name AS n ORDER BY n
		 RETURN collect(n) AS names, count(DISTINCT n) AS unique`, nil)

	r := res.Rows[0]
	fmt.Println(r[0], r[1])
	// Output: [Alice Bob Bob] 2
}

func ExampleDB_Query_scalarFunctions() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	res, _ := db.Query(context.Background(), `
		RETURN toUpper('alice') AS u,
		       substring('mycypher', 2, 4) AS s,
		       split('a,b,c', ',') AS parts,
		       size(split('a,b,c', ',')) AS n`, nil)

	r := res.Rows[0]
	fmt.Println(r[0], r[1], r[2], r[3])
	_ = ctx
	// Output: ALICE cyph [a b c] 3
}

func ExampleDB_Query_merge() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// Two MERGEs of the same key produce a single node.
	_, _ = db.Query(ctx, "MERGE (n:P {email: 'a@x.com'})", nil)
	_, _ = db.Query(ctx, "MERGE (n:P {email: 'a@x.com'})", nil)
	res, _ := db.Query(ctx, "MATCH (n:P) RETURN count(n) AS c", nil)

	fmt.Println(res.Rows[0][0])
	// Output: 1
}

func ExampleDB_Query_mergeOnCreateOnMatch() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// First call creates the node and fires ON CREATE.
	_, _ = db.Query(ctx,
		`MERGE (n:P {email: 'a@x.com'})
		 ON CREATE SET n.created = 1
		 ON MATCH SET n.seen = 1`, nil)
	// Second call matches and fires ON MATCH.
	_, _ = db.Query(ctx,
		`MERGE (n:P {email: 'a@x.com'})
		 ON CREATE SET n.created = 1
		 ON MATCH SET n.seen = 1`, nil)

	res, _ := db.Query(ctx, "MATCH (n:P) RETURN n.created AS c, n.seen AS s", nil)
	r := res.Rows[0]
	fmt.Println(r[0], r[1])
	// Output: 1 1
}

func ExampleDB_Query_setVariants() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (n:P {name: 'Alice', age: 30})", nil)
	// Single property.
	_, _ = db.Query(ctx, "MATCH (n:P) SET n.age = 31", nil)
	// Add a label.
	_, _ = db.Query(ctx, "MATCH (n:P) SET n:Admin", nil)
	// Merge (+= preserves other keys).
	_, _ = db.Query(ctx, "MATCH (n:P) SET n += {country: 'IT'}", nil)
	// Replace (= drops every other key).
	_, _ = db.Query(ctx, "MATCH (n:P) SET n = {name: 'Alice', score: 99}", nil)

	res, _ := db.Query(ctx,
		"MATCH (n:Admin) RETURN n.name AS name, n.score AS score, n.age AS age", nil)
	r := res.Rows[0]
	fmt.Println(r[0], r[1], r[2])
	// Output: Alice 99 <nil>
}

func ExampleDB_Query_removeAndDelete() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (:P {name: 'Alice', email: 'a@x.com'})", nil)
	_, _ = db.Query(ctx, "MATCH (n:P {name: 'Alice'}) SET n:Admin", nil)
	_, _ = db.Query(ctx, "CREATE (:P {name: 'Bob'})-[:KNOWS]->(:P {name: 'Carol'})", nil)

	// REMOVE strips a property and a label.
	_, _ = db.Query(ctx, "MATCH (n:P {name: 'Alice'}) REMOVE n.email, n:Admin", nil)
	res, _ := db.Query(ctx, "MATCH (n:Admin) RETURN count(n) AS c", nil)
	fmt.Println("admins:", res.Rows[0][0])

	// DETACH DELETE removes Bob plus the edge that involves him.
	_, _ = db.Query(ctx, "MATCH (n:P {name: 'Bob'}) DETACH DELETE n", nil)
	res, _ = db.Query(ctx, "MATCH (n:P) RETURN count(n) AS c", nil)
	fmt.Println("nodes:", res.Rows[0][0])
	// Output:
	// admins: 0
	// nodes: 2
}

func ExampleDB_Query_union() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (:Person {name: 'Alice'})", nil)
	_, _ = db.Query(ctx, "CREATE (:Person {name: 'Bob'})", nil)
	// "Alice" exists as both a Person and a Company.
	_, _ = db.Query(ctx, "CREATE (:Company {name: 'Acme'})", nil)
	_, _ = db.Query(ctx, "CREATE (:Company {name: 'Alice'})", nil)

	// UNION dedups; UNION ALL keeps duplicates.
	dedup, _ := db.Query(ctx,
		`MATCH (p:Person) RETURN p.name AS n
		 UNION
		 MATCH (c:Company) RETURN c.name AS n`, nil)
	all, _ := db.Query(ctx,
		`MATCH (p:Person) RETURN p.name AS n
		 UNION ALL
		 MATCH (c:Company) RETURN c.name AS n`, nil)

	fmt.Println("dedup:", len(dedup.Rows))
	fmt.Println("all:", len(all.Rows))
	// Output:
	// dedup: 3
	// all: 4
}

func ExampleDB_Query_createIndex() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, _ = db.Query(ctx, "CREATE (:Person {email: 'a@x.com', name: 'Alice'})", nil)
	_, _ = db.Query(ctx, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	res, _ := db.Query(ctx,
		"MATCH (p:Person {email: 'a@x.com'}) RETURN p.name AS n", nil)

	fmt.Println(res.Rows[0][0])
	// Output: Alice
}

// ExampleDB_Explain_withIndex shows EXPLAIN switching from a label scan to
// an index-backed lookup once the matching index exists.
func ExampleDB_Explain_withIndex() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	q := "MATCH (p:Person {email: 'a@x.com'}) RETURN p.name AS n"
	before, _ := db.Explain(ctx, q)
	_, _ = db.Query(ctx, "CREATE INDEX FOR (p:Person) ON (p.email)", nil)
	after, _ := db.Explain(ctx, q)

	fmt.Println("--- before ---")
	fmt.Print(before)
	fmt.Println("--- after ---")
	fmt.Print(after)
	// Output:
	// --- before ---
	// Project(p.name AS n)
	//   Filter(p.email = 'a@x.com')
	//     NodeByLabelScan(p:Person)
	// --- after ---
	// Project(p.name AS n)
	//   NodeByProperty(p:Person {email = 'a@x.com'})
}
