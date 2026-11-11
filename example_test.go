package intreccio_test

import (
	"context"
	"fmt"

	"github.com/giovibal/intreccio"
)

func ExampleDB_Query() {
	db, _ := intreccio.OpenInMemory()
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	_, _ = db.Query(ctx, "CREATE (n:Person {name: 'Alice'})", nil)
	res, _ := db.Query(ctx, "MATCH (p:Person) RETURN p.name AS name", nil)

	fmt.Println(res.Columns[0], "=", res.Rows[0][0])
	// Output: name = Alice
}

func ExampleDB_Explain() {
	db, _ := intreccio.OpenInMemory()
	defer func() { _ = db.Close() }()

	plan, _ := db.Explain(context.Background(), "MATCH (p:Person) RETURN p.name AS name")
	fmt.Print(plan)
	// Output:
	// Project(p.name AS name)
	//   NodeByLabelScan(p:Person)
}

func ExampleDB_Subscribe() {
	db, _ := intreccio.OpenInMemory()
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	_, _ = db.Query(ctx, "CREATE (:Person {name: 'Alice'})", nil)

	sub, _ := db.Subscribe(ctx, "MATCH (p:Person) RETURN p.name AS name", nil)
	defer func() { _ = sub.Close() }()

	// The first batch is the current result.
	first := <-sub.Changes()
	fmt.Println("initial:", first.Added)

	// Later batches describe how the result changed.
	_, _ = db.Query(ctx, "MATCH (p:Person) SET p.name = 'Alicia'", nil)
	change := <-sub.Changes()
	fmt.Println("removed:", change.Removed, "added:", change.Added)
	// Output:
	// initial: [[Alice]]
	// removed: [[Alice]] added: [[Alicia]]
}
