package intreccio

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestIndexCommands covers #35: named indexes, SHOW INDEXES and DROP INDEX.
func TestIndexCommands(t *testing.T) {
	ctx := context.Background()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	run := func(q string) *Result {
		t.Helper()
		res, err := db.Query(ctx, q, nil)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res
	}
	explain := func(q string) string {
		t.Helper()
		p, err := db.Explain(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	run("UNWIND [1, 2, 3] AS i CREATE (:P {uid: i, name: 'n' + toString(i)})")
	run("CREATE INDEX person_uid FOR (p:P) ON (p.uid)")
	run("CREATE INDEX FOR (p:P) ON (p.name)")
	run("CREATE INDEX FOR (p:P) ON (p.uid)") // same definition again: no-op

	res := run("SHOW INDEXES")
	if want := []string{"name", "label", "property"}; !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("columns %v, want %v", res.Columns, want)
	}
	want := [][]any{{"index_P_name", "P", "name"}, {"person_uid", "P", "uid"}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Errorf("SHOW INDEXES = %v, want %v", res.Rows, want)
	}

	if _, err := db.Query(ctx, "CREATE INDEX person_uid FOR (p:P) ON (p.name)", nil); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate name: got %v", err)
	}

	const lookup = "MATCH (p:P) WHERE p.uid = 2 RETURN p.name"
	if !strings.Contains(explain(lookup), "NodeByProperty") {
		t.Fatal("the index should be used before the drop")
	}
	run("DROP INDEX person_uid")
	if strings.Contains(explain(lookup), "NodeByProperty") {
		t.Error("the dropped index is still used")
	}
	if got := run(lookup).Rows; !reflect.DeepEqual(got, [][]any{{"n2"}}) {
		t.Errorf("data after drop: %v", got)
	}
	if got := run("SHOW INDEXES").Rows; !reflect.DeepEqual(got, [][]any{{"index_P_name", "P", "name"}}) {
		t.Errorf("SHOW INDEXES after drop = %v", got)
	}

	if _, err := db.Query(ctx, "DROP INDEX person_uid", nil); err == nil || !strings.Contains(err.Error(), "no index") {
		t.Errorf("dropping a missing index: got %v", err)
	}
	run("DROP INDEX person_uid IF EXISTS")
	run("DROP INDEX index_P_name") // the default name works too

	// Re-creating backfills again, and writes keep it consistent.
	run("CREATE INDEX FOR (p:P) ON (p.uid)")
	run("CREATE (:P {uid: 4, name: 'n4'})")
	if got := run("MATCH (p:P {uid: 4}) RETURN p.name").Rows; !reflect.DeepEqual(got, [][]any{{"n4"}}) {
		t.Errorf("lookup after re-create: %v", got)
	}
	if got := run("MATCH (p:P) WHERE p.uid >= 1 RETURN count(p)").Rows; !reflect.DeepEqual(got, [][]any{{int64(4)}}) {
		t.Errorf("range after re-create: %v", got)
	}

	for _, q := range []string{
		"MATCH (n) DROP INDEX x",
		"SHOW INDEXES RETURN 1",
		"DROP INDEX",
		"SHOW TABLES",
	} {
		if _, err := db.Query(ctx, q, nil); err == nil {
			t.Errorf("%s: expected an error", q)
		}
	}
}
