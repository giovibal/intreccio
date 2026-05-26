package mycypher_test

import (
	"context"
	"fmt"

	"github.com/giovibal/mycypher"
)

func ExampleDB_Query() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	_, _ = db.Query(ctx, "CREATE (n:Person {name: 'Alice'})", nil)
	res, _ := db.Query(ctx, "MATCH (p:Person) RETURN p.name AS name", nil)

	fmt.Println(res.Columns[0], "=", res.Rows[0][0])
	// Output: name = Alice
}

func ExampleDB_Explain() {
	db, _ := mycypher.OpenInMemory()
	defer func() { _ = db.Close() }()

	plan, _ := db.Explain(context.Background(), "MATCH (p:Person) RETURN p.name AS name")
	fmt.Print(plan)
	// Output:
	// Project(p.name AS name)
	//   NodeByLabelScan(p:Person)
}
