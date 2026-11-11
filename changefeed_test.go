package intreccio

import (
	"context"
	"testing"

	"github.com/giovibal/intreccio/internal/changefeed"
	"github.com/giovibal/intreccio/internal/storage"
)

func names(ns ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, n := range ns {
		out[n] = struct{}{}
	}
	return out
}

func isDirty(w *changefeed.Watcher) bool {
	select {
	case <-w.Dirty():
		return true
	default:
		return false
	}
}

// TestCommitsReachTheHub drives the embedded commit point end to end: Cypher
// writes are captured, resolved to names and matched against footprints.
func TestCommitsReachTheHub(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	run := func(q string) {
		t.Helper()
		if _, err := db.Query(ctx, q, nil); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	people := db.hub.Register(changefeed.Footprint{Labels: names("Person"), Keys: names("name")})
	knows := db.hub.Register(changefeed.Footprint{Types: names("KNOWS")})
	defer db.hub.Unregister(people)
	defer db.hub.Unregister(knows)

	run("CREATE (:Person {name: 'Ann', age: 1})")
	if !isDirty(people) || isDirty(knows) {
		t.Error("create person: wrong watchers woken")
	}

	run("CREATE (:Company {name: 'Acme'})")
	if isDirty(people) || isDirty(knows) {
		t.Error("create company: no watcher is concerned")
	}

	run("MATCH (p:Person) SET p.age = 2")
	if isDirty(people) {
		t.Error("an update of an unwatched key woke the watcher")
	}

	run("MATCH (p:Person) SET p.name = 'Anna'")
	if !isDirty(people) {
		t.Error("an update of a watched key did not wake the watcher")
	}

	run("MATCH (p:Person) SET p.name = 'Anna'")
	if isDirty(people) {
		t.Error("rewriting the same value woke the watcher")
	}

	run("MATCH (p:Person), (c:Company) CREATE (p)-[:KNOWS]->(c)")
	if isDirty(people) || !isDirty(knows) {
		t.Error("create relationship: wrong watchers woken")
	}

	run("CREATE INDEX FOR (p:Person) ON (p.name)")
	if isDirty(people) || isDirty(knows) {
		t.Error("an index backfill changes no record and must wake nobody")
	}

	run("MATCH (p:Person) DETACH DELETE p")
	if !isDirty(people) || !isDirty(knows) {
		t.Error("detach delete: both watchers are concerned")
	}

	// A failed statement commits nothing and reports nothing.
	if _, err := db.Query(ctx, "CREATE (:Person {name: $missing})", nil); err == nil {
		t.Fatal("expected an error for a missing parameter")
	}
	if isDirty(people) {
		t.Error("a failed write woke the watcher")
	}

	// Reads never report.
	run("MATCH (n) RETURN count(n)")
	if isDirty(people) || isDirty(knows) {
		t.Error("a read woke a watcher")
	}
}

// TestCommitWithoutCaptureWakesLateWatchers covers the window in which a
// watcher registers while a write that started without a capture is in flight:
// the commit cannot be described, so it must wake everyone.
func TestCommitWithoutCaptureWakesLateWatchers(t *testing.T) {
	db := newDB(t)
	be := db.be.(*localBackend)

	var late *changefeed.Watcher
	_, err := be.ApplyWrite(func(txn storage.Txn) (any, error) {
		// No watcher existed when the transaction began: no capture.
		if _, ok := txn.(*changefeed.Capture); ok {
			t.Error("a capture was installed although no watcher was registered")
		}
		late = db.hub.Register(changefeed.Footprint{Labels: names("Unrelated")})
		return nil, txn.Set([]byte("zz-test-key"), []byte{1})
	})
	if err != nil {
		t.Fatalf("ApplyWrite: %v", err)
	}
	defer db.hub.Unregister(late)
	if !isDirty(late) {
		t.Error("a commit without a capture must wake watchers registered meanwhile")
	}

	// With a watcher registered the next write is captured and filtered.
	_, err = be.ApplyWrite(func(txn storage.Txn) (any, error) {
		if _, ok := txn.(*changefeed.Capture); !ok {
			t.Error("no capture although a watcher is registered")
		}
		return nil, txn.Set([]byte("zz-test-key"), []byte{2})
	})
	if err != nil {
		t.Fatalf("ApplyWrite: %v", err)
	}
	if isDirty(late) {
		t.Error("a write touching no record woke the watcher")
	}
}
