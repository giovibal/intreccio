package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

// TestImportOnFollower: each batch of an Import on a follower is forwarded
// to the leader and replicated (#29).
func TestImportOnFollower(t *testing.T) {
	ctx := context.Background()
	tc := startCluster(t, 3)
	follower := tc.dbs[tc.aFollower(t)]
	names := make([]string, 250)
	for i := range names {
		names[i] = "p" + string(rune('a'+i%26))
	}
	res, err := follower.Import(ctx, "UNWIND $rows AS n CREATE (:Person {name: n})", names, nil, intreccio.WithBatchSize(100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 250 || res.Batches != 3 {
		t.Errorf("result = %+v", res)
	}
	for i := range tc.nodes {
		waitForCountLocal(t, tc.dbs[i], 250, 5*time.Second)
	}
}
