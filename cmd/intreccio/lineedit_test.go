package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestOneLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MATCH (n) RETURN n;", "MATCH (n) RETURN n;"},
		{"MATCH (n)\n  WHERE n.x = 1\n\tRETURN n;", "MATCH (n) WHERE n.x = 1 RETURN n;"},
		{"MATCH (n) // every node\nRETURN n; // done", "MATCH (n) RETURN n;"},
		{"MATCH (n) /* a\n   block */ RETURN n;", "MATCH (n) /* a block */ RETURN n;"},
		// Inside a string nothing is a comment, spacing is kept, and a line
		// break becomes the escape that stands for it.
		{"RETURN 'a // b' AS s;", "RETURN 'a // b' AS s;"},
		{"RETURN 'two  spaces' AS s;", "RETURN 'two  spaces' AS s;"},
		{"RETURN 'line one\nline two' AS s;", `RETURN 'line one\nline two' AS s;`},
		{`RETURN "it's \" fine" AS s,` + "\n 1 AS n;", `RETURN "it's \" fine" AS s, 1 AS n;`},
		{`RETURN 'ends with a backslash \\'` + "\n AS s;", `RETURN 'ends with a backslash \\' AS s;`},
		{"\n\n  RETURN 1;  \n", "RETURN 1;"},
		{"RETURN 1 / 2;", "RETURN 1 / 2;"},
	}
	for _, c := range cases {
		if got := oneLine(c.in); got != c.want {
			t.Errorf("oneLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHistory(t *testing.T) {
	h := &history{}
	if h.Len() != 0 || h.last() != "" {
		t.Fatalf("empty history: Len = %d, last = %q", h.Len(), h.last())
	}
	h.record("MATCH (n)\nRETURN n;")
	h.record("RETURN 1;")
	h.record("RETURN 1;") // an immediate repeat is not a new entry
	h.record("   ")       // nor is nothing at all
	h.Add("typed line")   // lines the terminal reports are not statements
	if h.Len() != 2 {
		t.Fatalf("Len = %d, want 2", h.Len())
	}
	// Index 0 is the most recent; entries are offered on one line.
	if h.At(0) != "RETURN 1;" || h.At(1) != "MATCH (n) RETURN n;" {
		t.Errorf("At = %q, %q", h.At(0), h.At(1))
	}
	if h.last() != "RETURN 1;" {
		t.Errorf("last = %q", h.last())
	}
	// The text as it was typed is found again from its one-line form.
	if got := h.original("MATCH (n) RETURN n;"); got != "MATCH (n)\nRETURN n;" {
		t.Errorf("original = %q", got)
	}
	if got := h.original("RETURN 2;"); got != "RETURN 2;" {
		t.Errorf("original of an unknown line = %q, want the line itself", got)
	}

	for i := range maxHistory + 10 {
		h.record(strings.Repeat("x", i+1) + ";")
	}
	if h.Len() != maxHistory {
		t.Errorf("Len = %d, want the cap %d", h.Len(), maxHistory)
	}
}

func TestKeyFilter(t *testing.T) {
	read := func(in string) (string, lineEvent) {
		f := &keyFilter{r: strings.NewReader(in)}
		var out []byte
		buf := make([]byte, 4) // smaller than an expansion: exercises the pending buffer
		for {
			n, err := f.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				return string(out), f.event
			}
		}
	}
	if got, ev := read("abc\x1b[A\r"); got != "abc\x1b[A\r" || ev != lineEntered {
		t.Errorf("plain input = %q, event %v", got, ev)
	}
	// Ctrl-C clears the line and submits it empty.
	if got, ev := read("abc\x03def"); got != "abc\x15\x0b\rdef" || ev != lineInterrupted {
		t.Errorf("Ctrl-C = %q, event %v", got, ev)
	}
	// Ctrl-X submits the line as it is, for the editor.
	if got, ev := read("abc\x18"); got != "abc\r" || ev != lineEdit {
		t.Errorf("Ctrl-X = %q, event %v", got, ev)
	}
	// CRLF and a bare LF are each one line ending.
	if got, _ := read("a\r\nb\nc"); got != "a\rb\rc" {
		t.Errorf("line endings = %q", got)
	}

	// Keys that arrive together are still handed over one line at a time, so a
	// flag set for one line cannot leak onto its neighbours.
	f := &keyFilter{r: strings.NewReader("one\rtwo\x18three\x03four\r")}
	buf := make([]byte, 64)
	for _, want := range []struct {
		bytes string
		event lineEvent
	}{
		{"one\r", lineEntered},
		{"two\r", lineEdit},
		{"three\x15\x0b\r", lineInterrupted},
		{"four\r", lineEntered},
	} {
		f.event = lineEntered
		n, err := f.Read(buf)
		if err != nil || string(buf[:n]) != want.bytes || f.event != want.event {
			t.Errorf("read %q (event %v, err %v), want %q (event %v)", buf[:n], f.event, err, want.bytes, want.event)
		}
	}
}

// keys runs the REPL over an interactive line source fed the given keystrokes,
// as a terminal in raw mode would deliver them, and returns what the REPL
// printed for query results and errors.
func keys(t *testing.T, input string) (out, errOut string) {
	t.Helper()
	db := newDB(t)
	var screen, results, errs bytes.Buffer
	src := newTermSource(strings.NewReader(input), &screen, nil, nil)
	replWith(db, src, &results, &errs)
	return results.String(), errs.String()
}

const (
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	ctrlC    = "\x03"
	ctrlD    = "\x04"
	ctrlX    = "\x18"
	enterKey = "\r"
)

func TestReplRecallsPreviousStatement(t *testing.T) {
	out, errOut := keys(t, ""+
		"CREATE (:P {n: 1});"+enterKey+
		keyUp+enterKey+ // run it again
		"MATCH (p:P) RETURN count(p) AS c;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if strings.Count(out, "OK") != 2 || !strings.Contains(out, "\n2\n") {
		t.Errorf("output = %q, want two OKs and a count of 2", out)
	}
}

func TestReplRecallEditsInPlace(t *testing.T) {
	// Recall, erase the last four characters ("1});"), type a new ending.
	out, errOut := keys(t, ""+
		"CREATE (:P {n: 1});"+enterKey+
		keyUp+"\x7f\x7f\x7f\x7f"+"7});"+enterKey+
		"MATCH (p:P) RETURN p.n AS n ORDER BY n;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if !strings.Contains(out, "\n1\n7\n") {
		t.Errorf("output = %q, want rows 1 and 7", out)
	}
}

func TestReplRecallsMultiLineStatementOnOneLine(t *testing.T) {
	out, errOut := keys(t, ""+
		"CREATE (:P {n: 1}) // first"+enterKey+
		"  ;"+enterKey+
		keyUp+enterKey+
		// Up twice, down once: still the same (only) entry kind, the older one.
		"MATCH (p:P)"+enterKey+"RETURN count(p) AS c;"+enterKey+
		keyUp+keyUp+keyDown+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if strings.Count(out, "OK") != 2 || strings.Count(out, "\n2\n") != 2 {
		t.Errorf("output = %q, want two OKs and the count of 2 twice", out)
	}
}

func TestReplCtrlCDiscardsInput(t *testing.T) {
	out, errOut := keys(t, ""+
		"CREATE (:Bad {n: 1})"+enterKey+ // a statement in progress...
		"garbage"+ctrlC+ // ...abandoned
		"MATCH (b:Bad) RETURN count(b) AS c;"+enterKey+
		ctrlC+ // at an empty prompt it does nothing
		"RETURN 'still here' AS s;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if !strings.Contains(out, "\n0\n") || !strings.Contains(out, "still here") {
		t.Errorf("output = %q, want a count of 0 and the last query's row", out)
	}
}

func TestReplEditOpensPreviousStatement(t *testing.T) {
	t.Setenv("VISUAL", "sed -i s/Alice/Bob/")
	out, errOut := keys(t, ""+
		"CREATE (:P {name: 'Alice'});"+enterKey+
		":edit"+enterKey+ // empty prompt: the previous statement, edited, runs
		"MATCH (p:P) RETURN p.name AS name ORDER BY name;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if !strings.Contains(out, "Alice\nBob\n") {
		t.Errorf("output = %q, want Alice and Bob", out)
	}
}

func TestReplEditedStatementBecomesThePreviousOne(t *testing.T) {
	// The first :edit turns x into y; the second starts from that result.
	t.Setenv("VISUAL", "sed -i -e s/y1/z1/ -e s/x1/y1/")
	out, errOut := keys(t, ""+
		"CREATE (:P {name: 'x1'});"+enterKey+
		":e"+enterKey+
		":e"+enterKey+
		keyUp+enterKey+ // and the arrow recalls it as well
		"MATCH (p:P) RETURN p.name AS name ORDER BY name;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	if !strings.Contains(out, "x1\ny1\nz1\nz1\n") {
		t.Errorf("output = %q, want x1, y1, z1, z1", out)
	}
}

func TestReplCtrlXOpensTheLineInTheEditor(t *testing.T) {
	t.Setenv("VISUAL", "sed -i s/Alice/Carol/")
	out, errOut := keys(t, ""+
		"CREATE (:P {name: 'Alice'}) // typed"+enterKey+";"+enterKey+
		keyUp+ctrlX+ // recall, then straight to the editor
		"CREATE (:Q {name: 'Alice'});"+ctrlX+ // or a line being typed
		"MATCH (n) RETURN labels(n)[0] + ':' + n.name AS n ORDER BY n;"+enterKey+
		ctrlD)
	if errOut != "" {
		t.Fatalf("errors: %q", errOut)
	}
	for _, want := range []string{"P:Alice", "P:Carol", "Q:Carol"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want %s", out, want)
		}
	}
	if strings.Contains(out, "Q:Alice") {
		t.Errorf("output = %q: the line sent to the editor was also run unedited", out)
	}
}

func TestReplEditWithNoHistoryOpensAnEmptyFile(t *testing.T) {
	t.Setenv("VISUAL", "true") // leaves the file as it is: empty
	out, errOut := keys(t, ":edit"+enterKey+"RETURN 1 AS one;"+enterKey+ctrlD)
	if errOut != "" || !strings.Contains(out, "\n1\n") {
		t.Errorf("output = %q, errors = %q", out, errOut)
	}
}

// TestReplEditOnPipedInput checks the same :edit behaviour when the REPL is not
// attached to a terminal.
func TestReplEditOnPipedInput(t *testing.T) {
	t.Setenv("VISUAL", "sed -i s/Alice/Bob/")
	db := newDB(t)
	var out, errOut bytes.Buffer
	input := strings.NewReader(strings.Join([]string{
		"CREATE (:P {name: 'Alice'});",
		":edit",
		"MATCH (p:P) RETURN p.name AS name ORDER BY name;",
		"",
	}, "\n"))
	repl(db, input, &out, &errOut)
	if errOut.Len() != 0 || !strings.Contains(out.String(), "Alice\nBob\n") {
		t.Errorf("output = %q, errors = %q", out.String(), errOut.String())
	}
}

func TestTermSourceReportsEndOfInput(t *testing.T) {
	var screen bytes.Buffer
	src := newTermSource(strings.NewReader("abc"+enterKey), &screen, nil, nil)
	if line, ev, err := src.readLine("> "); line != "abc" || ev != lineEntered || err != nil {
		t.Fatalf("first line = %q, %v, %v", line, ev, err)
	}
	if _, _, err := src.readLine("> "); err != io.EOF {
		t.Fatalf("at the end of input: err = %v, want io.EOF", err)
	}
	if !strings.Contains(screen.String(), "> ") {
		t.Errorf("the prompt was not drawn: %q", screen.String())
	}
}

// TestTermSourceIgnoresAZeroSize: a terminal that reports no size must not make
// the editor wrap after every character.
func TestTermSourceIgnoresAZeroSize(t *testing.T) {
	var screen bytes.Buffer
	zero := func() (int, int, error) { return 0, 0, nil }
	src := newTermSource(strings.NewReader("abcdef"+enterKey), &screen, nil, zero)
	if line, _, err := src.readLine("> "); line != "abcdef" || err != nil {
		t.Fatalf("line = %q, %v", line, err)
	}
	if got := screen.String(); !strings.Contains(got, "abcdef") {
		t.Errorf("the typed text was broken up on screen: %q", got)
	}
}
