package main

import "strings"

// maxHistory bounds the statements the REPL remembers.
const maxHistory = 500

// history is the REPL's statement history. An entry is a whole statement as it
// was typed (possibly over several lines) together with a one-line form, which
// is what the line editor offers when the user walks back through it.
//
// It implements term.History: index 0 is the most recent entry.
type history struct {
	entries []historyEntry // oldest first
}

type historyEntry struct {
	text string // as typed, terminator included
	line string // the same statement on one line
}

// record adds a statement that was run. Blank text and an immediate repeat of
// the previous statement are ignored.
func (h *history) record(text string) {
	text = strings.TrimSpace(text)
	line := oneLine(text)
	if line == "" {
		return
	}
	if n := len(h.entries); n > 0 && h.entries[n-1].line == line {
		h.entries[n-1].text = text // keep the latest spelling
		return
	}
	h.entries = append(h.entries, historyEntry{text: text, line: line})
	if len(h.entries) > maxHistory {
		h.entries = h.entries[len(h.entries)-maxHistory:]
	}
}

// last returns the most recent statement as it was typed, or "".
func (h *history) last() string {
	if len(h.entries) == 0 {
		return ""
	}
	return h.entries[len(h.entries)-1].text
}

// original returns the statement, as it was typed, whose one-line form is
// line: what a recalled line stands for. A line that is not a recalled one is
// returned as it is.
func (h *history) original(line string) string {
	line = strings.TrimSpace(line)
	for i := len(h.entries) - 1; i >= 0; i-- {
		if h.entries[i].line == line {
			return h.entries[i].text
		}
	}
	return line
}

// Add is called by the line editor for every line it reads. Lines are not
// statements — one statement may take several — so the REPL records statements
// itself and this does nothing.
func (h *history) Add(string) {}

// Len returns the number of entries.
func (h *history) Len() int { return len(h.entries) }

// At returns the one-line form of an entry; 0 is the most recent.
func (h *history) At(idx int) string { return h.entries[len(h.entries)-1-idx].line }

// oneLine puts a statement on a single line without changing what it means:
// runs of whitespace (line breaks included) become one space, `//` comments are
// dropped since they would swallow whatever follows them, and a line break
// inside a string literal becomes the \n escape that stands for it.
func oneLine(s string) string {
	var b strings.Builder
	space := func() {
		if n := b.Len(); n > 0 && b.String()[n-1] != ' ' {
			b.WriteByte(' ')
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`': // a literal, copied as it is
			quote := c
			b.WriteByte(c)
			for i++; i < len(s); i++ {
				c = s[i]
				switch {
				case c == '\\' && quote != '`' && i+1 < len(s):
					b.WriteByte(c)
					i++
					b.WriteByte(s[i])
					continue
				case c == '\n':
					b.WriteString(`\n`)
					continue
				case c == '\r':
					continue
				}
				b.WriteByte(c)
				if c == quote {
					break
				}
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '/': // to the end of the line
			for i < len(s) && s[i] != '\n' {
				i++
			}
			space()
		case c == '/' && i+1 < len(s) && s[i+1] == '*': // kept, on one line
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				end = len(s) - i - 2
			} else {
				end += 2
			}
			comment := s[i : i+2+end]
			b.WriteString(strings.Join(strings.Fields(comment), " "))
			i += len(comment) - 1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space()
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}
