package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/giovibal/mycypher"
)

func newDB(t *testing.T) *mycypher.DB {
	t.Helper()
	db, err := mycypher.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestExecuteWriteThenRead(t *testing.T) {
	db := newDB(t)
	var out bytes.Buffer
	if err := execute(db, "CREATE (n:Person {name: 'Bob'})", &out); err != nil {
		t.Fatalf("execute write: %v", err)
	}
	if got := out.String(); got != "OK\n" {
		t.Errorf("write output = %q, want \"OK\\n\"", got)
	}

	out.Reset()
	if err := execute(db, "MATCH (p:Person) RETURN p.name AS name", &out); err != nil {
		t.Fatalf("execute read: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "name") || !strings.Contains(got, "Bob") || !strings.Contains(got, "(1 row)") {
		t.Errorf("read output = %q", got)
	}
}

func TestExecuteSemanticError(t *testing.T) {
	db := newDB(t)
	var out bytes.Buffer
	err := execute(db, "MATCH (n) RETURN m", &out)
	if err == nil {
		t.Fatal("expected an error for undefined variable")
	}
	if out.Len() != 0 {
		t.Errorf("no output expected on error, got %q", out.String())
	}
}

func TestReplMultiLineStatement(t *testing.T) {
	db := newDB(t)
	var out, errOut bytes.Buffer
	input := strings.NewReader(strings.Join([]string{
		"CREATE",
		"  (n:Person {name: 'Alice'})",
		";",
		"MATCH (p:Person) RETURN p.name AS name;",
		":quit",
		"",
	}, "\n"))
	repl(db, input, &out, &errOut)

	got := out.String()
	if !strings.Contains(got, "OK") {
		t.Errorf("missing OK ack from CREATE: %q", got)
	}
	if !strings.Contains(got, "Alice") || !strings.Contains(got, "(1 row)") {
		t.Errorf("missing read result: %q", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected errors: %q", errOut.String())
	}
}

func TestReplExitOnEOF(t *testing.T) {
	db := newDB(t)
	var out, errOut bytes.Buffer
	// No terminator: at EOF we still execute the buffered statement.
	input := strings.NewReader("CREATE (n:Person {name: 'Solo'})\n")
	repl(db, input, &out, &errOut)
	if !strings.Contains(out.String(), "OK") {
		t.Errorf("expected OK at EOF, got %q", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected errors: %q", errOut.String())
	}
}

func TestRunBatch(t *testing.T) {
	db := newDB(t)
	var out, errOut bytes.Buffer
	runBatch(db,
		"CREATE (n:Person {name: 'A'});"+
			"CREATE (m:Person {name: 'B'});"+
			"MATCH (p:Person) RETURN p.name AS n ORDER BY n;",
		&out, &errOut)
	got := out.String()
	if !strings.Contains(got, "A") || !strings.Contains(got, "B") {
		t.Errorf("expected both A and B in output, got %q", got)
	}
	if !strings.Contains(got, "(2 rows)") {
		t.Errorf("expected 2 rows in MATCH output, got %q", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("no errors expected, got %q", errOut.String())
	}
}

func TestRunBatchSkipsEmptyAndContinuesOnError(t *testing.T) {
	db := newDB(t)
	var out, errOut bytes.Buffer
	// A leading ';' produces an empty statement (skipped). The middle "BOGUS"
	// statement fails to parse; the batch must still run the final MATCH.
	runBatch(db,
		"; CREATE (n:Person {name: 'A'}); BOGUS; MATCH (p:Person) RETURN p.name AS n;",
		&out, &errOut)
	got := out.String()
	if !strings.Contains(got, "A") {
		t.Errorf("expected A in output (final MATCH must run), got %q", got)
	}
	if errOut.Len() == 0 {
		t.Error("expected a parse error on stderr for BOGUS")
	}
}

func TestReplPropagatesErrors(t *testing.T) {
	db := newDB(t)
	var out, errOut bytes.Buffer
	input := strings.NewReader("MATCH (n) RETURN m;\n:quit\n")
	repl(db, input, &out, &errOut)
	if errOut.Len() == 0 {
		t.Error("expected an error on stderr for an unbound variable")
	}
}
