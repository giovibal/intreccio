package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/giovibal/mycypher"
)

func main() {
	cmdline := flag.String("c", "", "execute a single query and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mycypher [-c QUERY] [path]")
		fmt.Fprintln(os.Stderr, "If [path] is omitted, an in-memory database is opened.")
		flag.PrintDefaults()
	}
	flag.Parse()

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
		_, _ = fmt.Fprintln(out, "mycypher REPL — end statements with ';', type :quit to exit, :help for help")
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
	_, _ = fmt.Fprintln(out, "Commands: :quit, :exit, :help.")
	_, _ = fmt.Fprintln(out, "Note: ';' inside string literals is not recognized as a statement boundary.")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
