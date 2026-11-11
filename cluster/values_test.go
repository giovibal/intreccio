package cluster

import (
	"context"
	"testing"

	"github.com/giovibal/intreccio"
)

// TestForwardedPublicValues: nodes, relationships, paths and nested nulls
// survive forwarding to the leader (#66).
func TestForwardedPublicValues(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	lead := tc.dbs[tc.leaderIndex(t)]
	if _, err := lead.Query(ctx, "CREATE (:Person {name: 'Ann'})-[:KNOWS {since: 2020}]->(:Person {name: 'Bob'})", nil); err != nil {
		t.Fatal(err)
	}
	q := `MATCH (a {name: 'Ann'})-[r]->(b), p = shortestPath((a)-[*]-(b))
		RETURN a, r, p, [a, null] AS l, {x: null, n: b} AS m`
	for name, db := range map[string]*intreccio.DB{"follower": tc.dbs[tc.aFollower(t)], "client": tc.openClient(t, "client-values")} {
		res, err := db.Query(ctx, q, nil, intreccio.Linearizable())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		row := res.Rows[0]
		if a, ok := row[0].(intreccio.Node); !ok || a.Props["name"] != "Ann" || a.Labels[0] != "Person" {
			t.Errorf("%s: a = %#v", name, row[0])
		}
		if r, ok := row[1].(intreccio.Relationship); !ok || r.Type != "KNOWS" || r.Props["since"] != int64(2020) {
			t.Errorf("%s: r = %#v", name, row[1])
		}
		if p, ok := row[2].(intreccio.Path); !ok || len(p.Nodes) != 2 {
			t.Errorf("%s: p = %#v", name, row[2])
		}
		if l, ok := row[3].([]any); !ok || len(l) != 2 || l[1] != nil {
			t.Errorf("%s: l = %#v", name, row[3])
		}
		if m, ok := row[4].(map[string]any); !ok || m["x"] != nil || m["n"].(intreccio.Node).Props["name"] != "Bob" {
			t.Errorf("%s: m = %#v", name, row[4])
		}
	}
}
