package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"text/tabwriter"

	"github.com/giovibal/mycypher"
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
	return fmt.Sprintf("mycypher %s (commit %s, built %s)", buildVersion(), commit, date)
}

func main() {
	cmdline := flag.String("c", "", "execute a single query and exit")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mycypher [-c QUERY] [path]")
		fmt.Fprintln(os.Stderr, "If [path] is omitted, an in-memory database is opened.")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	db, err := openDB(flag.Arg(0))
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

	if flag.Arg(0) == "" && isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "(no path given: opened in-memory database)")
	}
	repl(db, os.Stdin, os.Stdout, os.Stderr)
}

func openDB(path string) (*mycypher.DB, error) {
	if path == "" {
		return mycypher.OpenInMemory()
	}
	return mycypher.Open(path)
}

// repl runs the read-eval-print loop, reading multi-line statements terminated
// by ';' from in and writing results to out / errors to errOut.
func repl(db *mycypher.DB, in io.Reader, out, errOut io.Writer) {
	tty := false
	if f, ok := in.(*os.File); ok {
		tty = isTerminal(f)
	}
	if tty {
		// _, _ = fmt.Fprintln(out, "mycypher REPL — end statements with ';', type :quit to exit, :help for help")
		printHelp(out)
	}

	reader := bufio.NewReader(in)
	var buf strings.Builder
	for {
		if tty {
			if buf.Len() == 0 {
				_, _ = fmt.Fprint(out, "> ")
			} else {
				_, _ = fmt.Fprint(out, ". ")
			}
		}
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			_, _ = fmt.Fprintln(errOut, err)
			return
		}
		eof := err == io.EOF
		line = strings.TrimRight(line, "\n\r")
		trimmed := strings.TrimSpace(line)

		// :edit/:e is handled regardless of buffer state: with a partial buffer
		// it pre-populates the editor with what was typed so far, then resets
		// the buffer and runs the edited content as a batch.
		if trimmed == ":edit" || trimmed == ":e" {
			initial := buf.String()
			buf.Reset()
			content, err := openEditor(initial)
			if err != nil {
				_, _ = fmt.Fprintln(errOut, err)
			} else {
				runBatch(db, content, out, errOut)
			}
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
			query := strings.TrimSpace(buf.String())
			query = strings.TrimRight(query, ";")
			query = strings.TrimSpace(query)
			buf.Reset()
			if query != "" {
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

	f, err := os.CreateTemp("", "mycypher-*.cypher")
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
func runBatch(db *mycypher.DB, content string, out, errOut io.Writer) {
	for _, stmt := range strings.Split(content, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := execute(db, stmt, out); err != nil {
			_, _ = fmt.Fprintln(errOut, err)
		}
	}
}

// execute runs a single query and writes the formatted result to out.
func execute(db *mycypher.DB, query string, out io.Writer) error {
	res, err := db.Query(context.Background(), query, nil)
	if err != nil {
		return err
	}
	printResult(out, res)
	return nil
}

func printResult(out io.Writer, res *mycypher.Result) {
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
	_, _ = fmt.Fprintln(out, "  :edit, :e       open the current buffer in $EDITOR and run the result")
	_, _ = fmt.Fprintln(out, "Note: ';' inside string literals is not recognized as a statement boundary.")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
