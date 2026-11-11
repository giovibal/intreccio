package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"text/tabwriter"

	"github.com/giovibal/intreccio"
)

// Build metadata. Overridden at release time via -ldflags "-X main.version=...";
// for a plain `go install ...@vX.Y.Z` the version falls back to the module
// version read from the embedded build info (see buildVersion).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// buildVersion returns the version string to report. When the binary was not
// stamped at release time (version == "dev"), it falls back to the module
// version recorded in the build info, so `go install ...@vX.Y.Z` still reports
// a meaningful version.
func buildVersion() string {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return version
}

// versionString formats the full one-line version banner.
func versionString() string {
	return fmt.Sprintf("intreccio %s (commit %s, built %s)", buildVersion(), commit, date)
}

func main() {
	cmdline := flag.String("c", "", "execute a single query and exit")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: intreccio [-c QUERY] [path]")
		fmt.Fprintln(os.Stderr, "If [path] is omitted, an in-memory database is opened.")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	db, clustered, err := openSelected(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	if *cmdline != "" {
		if err := execute(db, *cmdline, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if !clustered && flag.Arg(0) == "" && isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "(no path given: opened in-memory database)")
	}
	repl(db, os.Stdin, os.Stdout, os.Stderr)
}

// clusterOpen is set by the `cluster` build tag (cluster.go) to open a clustered
// DB. It stays nil in the default build, so the default binary never links the
// clustering package or Raft. It returns (db, true) when cluster mode is
// requested via flags.
var clusterOpen func() (*intreccio.DB, bool, error)

// openSelected opens a clustered DB when cluster flags are present (only in the
// `-tags cluster` build), otherwise a local one. The bool reports whether a
// clustered DB was opened.
func openSelected(path string) (*intreccio.DB, bool, error) {
	if clusterOpen != nil {
		if db, ok, err := clusterOpen(); ok || err != nil {
			return db, true, err
		}
	}
	db, err := openDB(path)
	return db, false, err
}

func openDB(path string) (*intreccio.DB, error) {
	if path == "" {
		return intreccio.OpenInMemory()
	}
	return intreccio.Open(path)
}

// repl runs the read-eval-print loop, reading multi-line statements terminated
// by ';' from in and writing results to out / errors to errOut. On a terminal
// it edits lines and keeps a statement history (see lineedit.go).
func repl(db *intreccio.DB, in io.Reader, out, errOut io.Writer) {
	replWith(db, newLineSource(in, out), out, errOut)
}

func replWith(db *intreccio.DB, src lineSource, out, errOut io.Writer) {
	if src.interactive() {
		printHelp(out)
	}
	hist := &history{}
	src.setHistory(hist)

	// edit opens text in the editor and runs what comes back, statement by
	// statement; each one becomes the previous statement in turn.
	edit := func(text string) {
		content, err := openEditor(text)
		if err != nil {
			_, _ = fmt.Fprintln(errOut, err)
			return
		}
		runStatements(db, content, out, errOut, hist)
	}

	var buf strings.Builder
	for {
		prompt := "> "
		if buf.Len() > 0 {
			prompt = ". "
		}
		line, event, err := src.readLine(prompt)
		if err != nil && err != io.EOF {
			_, _ = fmt.Fprintln(errOut, err)
			return
		}
		eof := err == io.EOF
		trimmed := strings.TrimSpace(line)

		switch event {
		case lineInterrupted: // Ctrl-C: drop whatever was being typed
			buf.Reset()
			continue
		case lineEdit: // Ctrl-X: what is on the prompt goes to the editor
			text := buf.String() + hist.original(line)
			buf.Reset()
			edit(text)
			continue
		}

		// :edit/:e is handled regardless of buffer state. With a partial
		// buffer it opens what was typed so far; on an empty prompt it opens
		// the previous statement. Either way the result is run.
		if trimmed == ":edit" || trimmed == ":e" {
			text := buf.String()
			buf.Reset()
			if strings.TrimSpace(text) == "" {
				text = hist.last()
			}
			edit(text)
			if eof {
				return
			}
			continue
		}

		if buf.Len() == 0 {
			switch trimmed {
			case "":
				if eof {
					return
				}
				continue
			case ":quit", ":exit", "quit", "exit":
				return
			case ":help":
				printHelp(out)
				if eof {
					return
				}
				continue
			}
		}

		if line != "" || buf.Len() > 0 {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}

		if strings.HasSuffix(trimmed, ";") || (eof && buf.Len() > 0) {
			text := strings.TrimSpace(buf.String())
			query := strings.TrimSpace(strings.TrimRight(text, ";"))
			buf.Reset()
			if query != "" {
				hist.record(text)
				if err := execute(db, query, out); err != nil {
					_, _ = fmt.Fprintln(errOut, err)
				}
			}
		}
		if eof {
			return
		}
	}
}

// openEditor writes initial to a temp file, runs $VISUAL/$EDITOR/vi on it, and
// returns the edited content. The editor inherits the terminal (stdin/stdout/stderr).
func openEditor(initial string) (string, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}

	f, err := os.CreateTemp("", "intreccio-*.cypher")
	if err != nil {
		return "", fmt.Errorf("edit: create temp: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if initial != "" {
		if _, err := f.WriteString(initial); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("edit: write temp: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("edit: close temp: %w", err)
	}

	cmd := exec.Command("sh", "-c", editor+` "$@"`, "--", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("edit: %s: %w", editor, err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit: read temp: %w", err)
	}
	return string(content), nil
}

// runBatch splits content on ';' and runs each non-empty statement in order.
// Errors are reported on errOut but do not interrupt the batch.
func runBatch(db *intreccio.DB, content string, out, errOut io.Writer) {
	runStatements(db, content, out, errOut, nil)
}

// runStatements runs the ';'-separated statements of content, recording each
// in hist when there is one.
func runStatements(db *intreccio.DB, content string, out, errOut io.Writer, hist *history) {
	for _, stmt := range strings.Split(content, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if hist != nil {
			hist.record(stmt + ";")
		}
		if err := execute(db, stmt, out); err != nil {
			_, _ = fmt.Fprintln(errOut, err)
		}
	}
}

// execute runs a single query and writes the formatted result to out.
func execute(db *intreccio.DB, query string, out io.Writer) error {
	res, err := db.Query(context.Background(), query, nil)
	if errors.Is(err, intreccio.ErrStreamQuery) {
		// A STREAM query is a live query: follow it until interrupted.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return stream(ctx, db, query, out)
	}
	if err != nil {
		return err
	}
	printResult(out, res)
	return nil
}

// stream runs a live query, printing its current result and then every change
// to it until ctx is done. Rows entering the result are prefixed with '+', rows
// leaving it with '-'; a changed row shows as a '-' line followed by a '+' line.
func stream(ctx context.Context, db *intreccio.DB, query string, out io.Writer) error {
	sub, err := db.Subscribe(ctx, query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Close() }()

	for c := range sub.Changes() {
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		if c.Initial {
			cols := sub.Columns()
			sep := make([]string, len(cols))
			for i, col := range cols {
				sep[i] = strings.Repeat("-", len(col))
			}
			_, _ = fmt.Fprintln(w, "  "+strings.Join(cols, "\t"))
			_, _ = fmt.Fprintln(w, "  "+strings.Join(sep, "\t"))
		}
		printSigned(w, "- ", c.Removed)
		printSigned(w, "+ ", c.Added)
		_ = w.Flush()
		if c.Initial {
			plural := "s"
			if len(c.Added) == 1 {
				plural = ""
			}
			_, _ = fmt.Fprintf(out, "(%d row%s; live - press Ctrl-C to stop)\n", len(c.Added), plural)
		}
	}
	if err := sub.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	_, _ = fmt.Fprintln(out, "(stream stopped)")
	return nil
}

func printSigned(w io.Writer, sign string, rows [][]any) {
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = renderValue(v)
		}
		_, _ = fmt.Fprintln(w, sign+strings.Join(parts, "\t"))
	}
}

func printResult(out io.Writer, res *intreccio.Result) {
	if len(res.Columns) == 0 {
		_, _ = fmt.Fprintln(out, "OK")
		return
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(res.Columns, "\t"))
	sep := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		sep[i] = strings.Repeat("-", len(c))
	}
	_, _ = fmt.Fprintln(w, strings.Join(sep, "\t"))
	for _, row := range res.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = renderValue(v)
		}
		_, _ = fmt.Fprintln(w, strings.Join(parts, "\t"))
	}
	_ = w.Flush()
	plural := "s"
	if len(res.Rows) == 1 {
		plural = ""
	}
	_, _ = fmt.Fprintf(out, "(%d row%s)\n", len(res.Rows), plural)
}

func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool, int64, float64:
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func printHelp(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Statements end with ';' and may span multiple lines.")
	_, _ = fmt.Fprintln(out, "Commands:")
	_, _ = fmt.Fprintln(out, "  :quit, :exit    leave the REPL")
	_, _ = fmt.Fprintln(out, "  :help           show this message")
	_, _ = fmt.Fprintln(out, "  :edit, :e       open the statement being typed, or the previous one, in $EDITOR and run the result")
	_, _ = fmt.Fprintln(out, "Keys: Up/Down recall earlier statements, Ctrl-X opens the line in $EDITOR,")
	_, _ = fmt.Fprintln(out, "  Ctrl-C discards what is being typed, Ctrl-D leaves the REPL.")
	_, _ = fmt.Fprintln(out, "Live queries: prefix a read query with STREAM to follow its result;")
	_, _ = fmt.Fprintln(out, "  rows are printed with '+' when they appear and '-' when they go. Ctrl-C stops it.")
	_, _ = fmt.Fprintln(out, "Note: ';' inside string literals is not recognized as a statement boundary.")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
