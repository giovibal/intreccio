package intreccio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func expectViolation(t *testing.T, db *DB, cypher string) {
	t.Helper()
	if _, err := db.Query(context.Background(), cypher, nil); !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("%s: err = %v, want ErrConstraintViolation", cypher, err)
	}
}

func countOf(t *testing.T, db *DB, cypher string) int64 {
	t.Helper()
	return mustQuery(t, db, cypher, nil).Rows[0][0].(int64)
}

// TestUniqueConstraintWritePaths covers #37: every way of giving a node a
// value is checked, and every way of dropping one frees it.
func TestUniqueConstraintWritePaths(t *testing.T) {
	db := newDB(t)
	mustQuery(t, db, "CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS UNIQUE", nil)
	mustQuery(t, db, "CREATE (:User {email: 'a'}), (:User {email: 'b'})", nil)

	// CREATE, in one statement or across statements; nothing is committed.
	expectViolation(t, db, "CREATE (:User {email: 'a'})")
	expectViolation(t, db, "CREATE (:User {email: 'c'}), (:User {email: 'c'})")
	expectViolation(t, db, "UNWIND [1, 2] AS i CREATE (:User {email: 'd'})")
	if n := countOf(t, db, "MATCH (u:User) RETURN count(u)"); n != 2 {
		t.Fatalf("users = %d after failed creates, want 2", n)
	}

	// SET (property, += and = map), and REMOVE / SET null freeing the value.
	expectViolation(t, db, "MATCH (u:User {email: 'b'}) SET u.email = 'a'")
	expectViolation(t, db, "MATCH (u:User {email: 'b'}) SET u += {email: 'a'}")
	expectViolation(t, db, "MATCH (u:User {email: 'b'}) SET u = {email: 'a'}")
	mustQuery(t, db, "MATCH (u:User {email: 'a'}) SET u.email = 'a'", nil) // same node: fine
	mustQuery(t, db, "MATCH (u:User {email: 'a'}) SET u.email = 'a2'", nil)
	mustQuery(t, db, "CREATE (:User {email: 'a'})", nil) // 'a' was freed
	mustQuery(t, db, "MATCH (u:User {email: 'a'}) REMOVE u.email", nil)
	mustQuery(t, db, "MATCH (u:User {email: 'a2'}) SET u.email = null", nil)
	mustQuery(t, db, "CREATE (:User {email: 'a'}), (:User {email: 'a2'})", nil)
	mustQuery(t, db, "MATCH (u:User {email: 'b'}) SET u = {name: 'no email'}", nil)
	mustQuery(t, db, "CREATE (:User {email: 'b'})", nil)

	// Labels: adding the label claims the value, removing it frees it.
	mustQuery(t, db, "CREATE (:Guest {email: 'a'})", nil) // other label: unconstrained
	expectViolation(t, db, "MATCH (g:Guest {email: 'a'}) SET g:User")
	mustQuery(t, db, "MATCH (u:User {email: 'a'}) REMOVE u:User", nil)
	mustQuery(t, db, "MATCH (g:Guest {email: 'a'}) WHERE NOT g:User SET g:User", nil)

	// DELETE frees the value.
	mustQuery(t, db, "MATCH (u:User {email: 'b'}) DELETE u", nil)
	mustQuery(t, db, "CREATE (:User {email: 'b'})", nil)

	// 1 and 1.0 are equal values, as for = and index lookups.
	mustQuery(t, db, "CREATE (:User {email: 1})", nil)
	expectViolation(t, db, "CREATE (:User {email: 1.0})")

	// MERGE matches instead of duplicating.
	mustQuery(t, db, "MERGE (:User {email: 'm'})", nil)
	mustQuery(t, db, "MERGE (:User {email: 'm'})", nil)
	if n := countOf(t, db, "MATCH (u:User {email: 'm'}) RETURN count(u)"); n != 1 {
		t.Errorf("merged users = %d, want 1", n)
	}

	// Error message names the label, property and value.
	_, err := db.Query(context.Background(), "CREATE (:User {email: 'm'})", nil)
	if err == nil || !strings.Contains(err.Error(), `:User node already has email = "m"`) {
		t.Errorf("violation message = %v", err)
	}
}

func TestUniqueConstraintLifecycle(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustQuery(t, db, "CREATE (:P {k: 1}), (:P {k: 1}), (:P {k: 2})", nil)

	// Existing duplicates: refused, and nothing is left behind.
	expectViolation(t, db, "CREATE CONSTRAINT FOR (p:P) REQUIRE p.k IS UNIQUE")
	if rows := mustQuery(t, db, "SHOW CONSTRAINTS", nil).Rows; len(rows) != 0 {
		t.Fatalf("constraints after a failed create = %v", rows)
	}
	if rows := mustQuery(t, db, "SHOW INDEXES", nil).Rows; len(rows) != 0 {
		t.Fatalf("indexes after a failed create = %v", rows)
	}

	// Without an index: the constraint creates one and drops it with itself.
	mustQuery(t, db, "MATCH (p:P {k: 1}) WITH p LIMIT 1 DELETE p", nil)
	mustQuery(t, db, "CREATE CONSTRAINT p_k IF NOT EXISTS FOR (p:P) REQUIRE p.k IS UNIQUE", nil)
	mustQuery(t, db, "CREATE CONSTRAINT FOR (p:P) REQUIRE p.k IS UNIQUE", nil) // idempotent
	if _, err := db.Query(ctx, "CREATE CONSTRAINT other FOR (p:P) REQUIRE p.k IS UNIQUE", nil); err == nil {
		t.Error("same constraint under another name succeeded")
	}
	if _, err := db.Query(ctx, "CREATE CONSTRAINT p_k FOR (q:Q) REQUIRE q.k IS UNIQUE", nil); err == nil {
		t.Error("duplicate constraint name succeeded")
	}
	got := fmt.Sprint(mustQuery(t, db, "SHOW CONSTRAINTS", nil).Rows)
	if got != "[[p_k P k UNIQUE]]" {
		t.Errorf("SHOW CONSTRAINTS = %s", got)
	}
	if got := fmt.Sprint(mustQuery(t, db, "SHOW INDEXES", nil).Rows); got != "[[index_P_k P k]]" {
		t.Errorf("SHOW INDEXES = %s, want the backing index", got)
	}
	if _, err := db.Query(ctx, "DROP INDEX index_P_k", nil); err == nil || !strings.Contains(err.Error(), "backs constraint") {
		t.Errorf("dropping the backing index = %v, want refusal", err)
	}
	plan, err := db.Explain(ctx, "MATCH (p:P {k: 2}) RETURN p")
	if err != nil || !strings.Contains(plan, "NodeByProperty") {
		t.Errorf("lookup on a constrained property does not use the index: %v\n%s", err, plan)
	}
	mustQuery(t, db, "DROP CONSTRAINT p_k", nil)
	mustQuery(t, db, "DROP CONSTRAINT p_k IF EXISTS", nil)
	if _, err := db.Query(ctx, "DROP CONSTRAINT p_k", nil); err == nil {
		t.Error("dropping a missing constraint succeeded")
	}
	if rows := mustQuery(t, db, "SHOW INDEXES", nil).Rows; len(rows) != 0 {
		t.Errorf("owned index survived the constraint: %v", rows)
	}
	mustQuery(t, db, "CREATE (:P {k: 2})", nil) // no longer enforced

	// With an existing index: the constraint uses it and leaves it behind.
	mustQuery(t, db, "CREATE INDEX FOR (n:Q) ON (n.k)", nil)
	mustQuery(t, db, "CREATE CONSTRAINT FOR (q:Q) REQUIRE q.k IS UNIQUE", nil)
	mustQuery(t, db, "CREATE (:Q {k: 1})", nil)
	expectViolation(t, db, "CREATE (:Q {k: 1})")
	mustQuery(t, db, "DROP CONSTRAINT constraint_Q_k", nil)
	if got := fmt.Sprint(mustQuery(t, db, "SHOW INDEXES", nil).Rows); got != "[[index_Q_k Q k]]" {
		t.Errorf("SHOW INDEXES = %s, want the pre-existing index kept", got)
	}
	mustQuery(t, db, "CREATE (:Q {k: 1})", nil)

	// Recreating a constraint after a drop starts from the current data.
	expectViolation(t, db, "CREATE CONSTRAINT FOR (q:Q) REQUIRE q.k IS UNIQUE")
}

func TestUniqueConstraintSyntaxErrors(t *testing.T) {
	db := newDB(t)
	for _, q := range []string{
		"CREATE CONSTRAINT FOR (u:User) REQUIRE v.email IS UNIQUE",
		"CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS NOT NULL",
		"CREATE CONSTRAINT FOR (u:User) REQUIRE u.email",
		"CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS UNIQUE RETURN 1",
		"DROP CONSTRAINT",
		"SHOW CONSTRAINTS RETURN 1",
	} {
		if _, err := db.Query(context.Background(), q, nil); err == nil {
			t.Errorf("%s: succeeded, want an error", q)
		}
	}
}

// TestUniqueConstraintPersistsAndConcurrent: on disk, concurrent writers
// racing for one value yield exactly one node, and the constraint survives
// a reopen.
func TestUniqueConstraintPersistsAndConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustQuery(t, db, "CREATE CONSTRAINT FOR (u:User) REQUIRE u.email IS UNIQUE", nil)
	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok, dup int
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Query(ctx, "CREATE (:User {email: 'race'})", nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrConstraintViolation):
				dup++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || dup != writers-1 {
		t.Errorf("successes = %d, violations = %d; want 1 and %d", ok, dup, writers-1)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := countOf(t, db, "MATCH (u:User) RETURN count(u)"); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	expectViolation(t, db, "CREATE (:User {email: 'race'})")
}
