package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giovibal/intreccio"
)

func newDB(t *testing.T) *intreccio.DB {
	t.Helper()
	db, err := intreccio.OpenInMemory()
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

func TestVersionString(t *testing.T) {
	got := versionString()
	if !strings.HasPrefix(got, "intreccio ") {
		t.Errorf("version string = %q, want prefix %q", got, "intreccio ")
	}
	for _, want := range []string{buildVersion(), "commit " + commit, "built " + date} {
		if !strings.Contains(got, want) {
			t.Errorf("version string %q missing %q", got, want)
		}
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

func TestRenderValue(t *testing.T) {
	node := intreccio.Node{ID: 1, Labels: []string{"Admin", "Person"}, Props: map[string]any{"name": "Ann", "age": int64(30)}}
	rel := intreccio.Relationship{ID: 2, Type: "KNOWS", Start: 1, End: 3, Props: map[string]any{"since": int64(2020)}}
	cases := []struct {
		in   any
		want string
	}{
		{nil, "null"},
		{"plain", "plain"},
		{int64(7), "7"},
		{1.5, "1.5"},
		{true, "true"},
		{[]any{int64(1), "a", nil}, `[1, "a", null]`},
		{map[string]any{"b": int64(2), "a": "x"}, `{a: "x", b: 2}`},
		{node, `(:Admin:Person {age: 30, name: "Ann"})`},
		{intreccio.Node{ID: 4, Props: map[string]any{}}, "()"},
		{intreccio.Node{ID: 5, Props: map[string]any{"k": true}}, "({k: true})"},
		{rel, "[:KNOWS {since: 2020}]"},
		{intreccio.Relationship{ID: 6, Type: "T", Props: map[string]any{}}, "[:T]"},
		{[]any{node, rel}, `[(:Admin:Person {age: 30, name: "Ann"}), [:KNOWS {since: 2020}]]`},
	}
	for _, c := range cases {
		if got := renderValue(c.in); got != c.want {
			t.Errorf("renderValue(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecuteReturnsWholeNode(t *testing.T) {
	db := newDB(t)
	var out bytes.Buffer
	if err := execute(db, "CREATE (n:Person {name: 'Bob'}) RETURN n", &out); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, `(:Person {name: "Bob"})`) {
		t.Errorf("output = %q, want the node rendered as a pattern", got)
	}
}

// syncBuffer is a bytes.Buffer safe to write from one goroutine and read from
// another.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForOutput(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%s", want, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamPrintsInitialResultThenChanges(t *testing.T) {
	db := newDB(t)
	var sink bytes.Buffer
	if err := execute(db, "CREATE (:Person {name: 'Ann'})", &sink); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- stream(ctx, db, "STREAM MATCH (p:Person) RETURN p.name AS name", out) }()

	waitForOutput(t, out, "(1 row; live - press Ctrl-C to stop)")
	if got := out.String(); !strings.Contains(got, "  name") || !strings.Contains(got, "+ Ann") {
		t.Errorf("initial output = %q", got)
	}

	if err := execute(db, "CREATE (:Person {name: 'Bob'})", &sink); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, out, "+ Bob")

	if err := execute(db, "MATCH (p:Person {name: 'Ann'}) SET p.name = 'Anna'", &sink); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, out, "+ Anna")
	if got := out.String(); strings.Index(got, "- Ann\n") < 0 || strings.Index(got, "- Ann\n") > strings.Index(got, "+ Anna") {
		t.Errorf("an update must print the old row before the new one:\n%s", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stream returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
	if got := out.String(); !strings.HasSuffix(got, "(stream stopped)\n") {
		t.Errorf("final output = %q", got)
	}
}

func TestStreamReportsQueryErrors(t *testing.T) {
	db := newDB(t)
	var out bytes.Buffer
	if err := stream(context.Background(), db, "STREAM CREATE (n) RETURN n", &out); err == nil {
		t.Error("stream of a writing query: expected an error")
	}
	if err := stream(context.Background(), db, "STREAM MATCH (n) RETURN m", &out); err == nil {
		t.Error("stream of an invalid query: expected an error")
	}
}
