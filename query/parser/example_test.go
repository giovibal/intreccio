package parser_test

import (
	"fmt"

	"github.com/giovibal/intreccio/query/parser"
	"github.com/giovibal/intreccio/query/plan"
)

// Example shows the openCypher front-end used on its own, with no database:
// parse a query into an AST, then build and render its physical plan. Callers
// without an index catalog pass plan.NoIndexes.
func Example() {
	q, err := parser.Parse("MATCH (p:Person) WHERE p.age > 30 RETURN p.name AS name")
	if err != nil {
		panic(err)
	}

	op, err := plan.Plan(q, plan.NoIndexes)
	if err != nil {
		panic(err)
	}

	fmt.Print(plan.Explain(op))
	// Output:
	// Project(p.name AS name)
	//   Filter(p.age > 30)
	//     NodeByLabelScan(p:Person)
}
