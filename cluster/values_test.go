package cluster

import (
	"context"
	"reflect"
	"testing"

	"github.com/giovibal/intreccio"
)

// TestForwardedEntityValues verifies that whole nodes and relationships, nulls
// and nested values survive forwarding unchanged: a follower's forwarded write
// and linearizable read, and a dataless client's read, all return the same rows
// as the leader's local execution.
func TestForwardedEntityValues(t *testing.T) {
	tc := startCluster(t, 3)
	leader := tc.leaderIndex(t)
	follower := tc.aFollower(t)
	ctx := context.Background()

	// A forwarded write that returns the entities it created.
	res, err := tc.dbs[follower].Query(ctx,
		"CREATE (a:Person:Admin {name: 'Ann', tags: ['x', 'y']})-[r:KNOWS {since: 2020}]->(b:Person {name: 'Bob'}) RETURN a, r, b", nil)
	if err != nil {
		t.Fatalf("forwarded write: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("forwarded write rows = %d, want 1", len(res.Rows))
	}
	a, okA := res.Rows[0][0].(intreccio.Node)
	r, okR := res.Rows[0][1].(intreccio.Relationship)
	b, okB := res.Rows[0][2].(intreccio.Node)
	if !okA || !okR || !okB {
		t.Fatalf("forwarded write row = %#v, want (Node, Relationship, Node)", res.Rows[0])
	}
	if want := []string{"Admin", "Person"}; !reflect.DeepEqual(a.Labels, want) {
		t.Errorf("a.Labels = %v, want %v", a.Labels, want)
	}
	if want := map[string]any{"name": "Ann", "tags": []any{"x", "y"}}; !reflect.DeepEqual(a.Props, want) {
		t.Errorf("a.Props = %#v, want %#v", a.Props, want)
	}
	if r.Type != "KNOWS" || r.Start != a.ID || r.End != b.ID || r.Props["since"] != int64(2020) {
		t.Errorf("r = %+v", r)
	}

	const q = "MATCH (a:Person)-[r:KNOWS]->(b) OPTIONAL MATCH (b)-[:NONE]->(c) " +
		"RETURN a, r, collect(b) AS bs, c, {who: a, n: 1} AS m"
	want, err := tc.dbs[leader].Query(ctx, q, nil)
	if err != nil {
		t.Fatalf("leader read: %v", err)
	}
	if len(want.Rows) != 1 || want.Rows[0][3] != nil {
		t.Fatalf("leader rows = %#v, want one row with a null c", want.Rows)
	}

	got, err := tc.dbs[follower].Query(ctx, q, nil, intreccio.Linearizable())
	if err != nil {
		t.Fatalf("forwarded linearizable read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwarded read = %#v, want %#v", got.Rows, want.Rows)
	}

	clientDB := tc.openClient(t, "client-values")
	got, err = clientDB.Query(ctx, q, nil, intreccio.Linearizable())
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("client read = %#v, want %#v", got.Rows, want.Rows)
	}

	// A forwarded write with no RETURN still yields no rows.
	res, err = tc.dbs[follower].Query(ctx, "CREATE (:Person {name: 'Cy'})", nil)
	if err != nil {
		t.Fatalf("forwarded write without RETURN: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("rows = %#v, want none", res.Rows)
	}
}
