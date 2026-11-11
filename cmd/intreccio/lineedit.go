package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// lineEvent says how a line of input ended.
type lineEvent int

const (
	// lineEntered: the user pressed Enter.
	lineEntered lineEvent = iota
	// lineInterrupted: the user pressed Ctrl-C; whatever was being typed is
	// to be discarded.
	lineInterrupted
	// lineEdit: the user pressed Ctrl-X; the line is to be opened in the
	// editor instead of being run.
	lineEdit
)

// lineSource is where the REPL reads its lines from.
type lineSource interface {
	// readLine shows prompt and returns the next line, without its line
	// ending. At the end of input it returns io.EOF, possibly with a last
	// unterminated line.
	readLine(prompt string) (string, lineEvent, error)
	// interactive reports whether a person is typing.
	interactive() bool
	// setHistory gives the source the statements to offer on the arrow keys.
	setHistory(h *history)
}

// newLineSource picks the line source for the REPL: a line editor with history
// when both ends are a terminal, plain line reading otherwise (piped input, or
// a terminal that cannot be put in raw mode).
func newLineSource(in io.Reader, out io.Writer) lineSource {
	fin, inFile := in.(*os.File)
	fout, outFile := out.(*os.File)
	tty := inFile && term.IsTerminal(int(fin.Fd()))
	if tty && outFile && term.IsTerminal(int(fout.Fd())) {
		fd := int(fin.Fd())
		raw := func() (func(), error) {
			state, err := term.MakeRaw(fd)
			if err != nil {
				return nil, err
			}
			return func() { _ = term.Restore(fd, state) }, nil
		}
		if restore, err := raw(); err == nil {
			restore()
			size := func() (int, int, error) { return term.GetSize(int(fout.Fd())) }
			return newTermSource(fin, fout, raw, size)
		}
	}
	return &plainSource{r: bufio.NewReader(in), out: out, prompts: tty}
}

// plainSource reads lines as they come, with no editing.
type plainSource struct {
	r       *bufio.Reader
	out     io.Writer
	prompts bool
}

func (s *plainSource) readLine(prompt string) (string, lineEvent, error) {
	if s.prompts {
		_, _ = io.WriteString(s.out, prompt)
	}
	line, err := s.r.ReadString('\n')
	return strings.TrimRight(line, "\n\r"), lineEntered, err
}

func (s *plainSource) interactive() bool   { return s.prompts }
func (s *plainSource) setHistory(*history) {}

// termSource is the interactive line editor: cursor movement, and the
// statement history on the up and down arrows.
//
// The terminal is in raw mode only while a line is being read. Everything else
// — running a statement, following a live query, the external editor — happens
// with the terminal as the user left it, so Ctrl-C keeps interrupting those.
type termSource struct {
	t    *term.Terminal
	keys *keyFilter
	out  io.Writer
	// raw puts the terminal in raw mode and returns how to undo it; nil when
	// there is no terminal to configure (tests).
	raw func() (restore func(), err error)
	// size returns the terminal's width and height; nil when unknown.
	size func() (width, height int, err error)
}

func newTermSource(in io.Reader, out io.Writer, raw func() (func(), error), size func() (int, int, error)) *termSource {
	keys := &keyFilter{r: in}
	return &termSource{
		t: term.NewTerminal(struct {
			io.Reader
			io.Writer
		}{keys, out}, ""),
		keys: keys,
		out:  out,
		raw:  raw,
		size: size,
	}
}

func (s *termSource) interactive() bool { return true }

func (s *termSource) setHistory(h *history) { s.t.History = h }

func (s *termSource) readLine(prompt string) (string, lineEvent, error) {
	if s.raw != nil {
		restore, err := s.raw()
		if err != nil {
			return "", lineEntered, err
		}
		defer restore()
	}
	if s.size != nil {
		// A terminal that reports no size (some pseudo-terminals do) keeps the
		// editor's default: with a width of zero it would wrap every character.
		if w, h, err := s.size(); err == nil && w > 0 && h > 0 {
			_ = s.t.SetSize(w, h)
		}
	}
	s.t.SetPrompt(prompt)
	s.keys.event = lineEntered
	line, err := s.t.ReadLine()
	if errors.Is(err, term.ErrPasteIndicator) {
		err = nil // a pasted line is a line like any other
	}
	if err == io.EOF {
		_, _ = io.WriteString(s.out, "\r\n") // leave the prompt's line
	}
	return line, s.keys.event, err
}

const (
	keyCtrlC = 0x03
	keyCtrlK = 0x0b // delete to the end of the line
	keyCtrlU = 0x15 // delete to the start of the line
	keyCtrlX = 0x18
)

// keyFilter sits between the terminal and the line editor and gives two keys
// the meaning the REPL wants. The editor treats Ctrl-C as the end of input and
// has no binding to hand a line over, so:
//
//   - Ctrl-C becomes "clear the line and submit it", flagged lineInterrupted;
//   - Ctrl-X becomes "submit the line", flagged lineEdit.
//
// event holds the flag of the line being read; the source resets it before
// each line. To keep the flag on the right line when keys arrive in bulk
// (typing ahead, pasting), the filter never hands the editor anything past the
// end of the current line: what follows waits, untranslated, for the next one.
type keyFilter struct {
	r       io.Reader
	event   lineEvent
	raw     []byte // input read but not yet translated
	pending []byte // translated bytes of the current line not yet handed out
}

func (f *keyFilter) Read(p []byte) (int, error) {
	if len(f.pending) == 0 {
		if len(f.raw) == 0 {
			buf := make([]byte, 256)
			n, err := f.r.Read(buf)
			if n == 0 {
				return 0, err
			}
			f.raw = buf[:n]
		}
		f.translateLine()
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

// translateLine moves input from raw to pending, up to and including the end
// of one line.
func (f *keyFilter) translateLine() {
	for len(f.raw) > 0 {
		c := f.raw[0]
		f.raw = f.raw[1:]
		switch c {
		case keyCtrlC:
			f.event = lineInterrupted
			f.pending = append(f.pending, keyCtrlU, keyCtrlK, '\r')
			return
		case keyCtrlX:
			f.event = lineEdit
			f.pending = append(f.pending, '\r')
			return
		case '\r', '\n':
			if c == '\r' && len(f.raw) > 0 && f.raw[0] == '\n' {
				f.raw = f.raw[1:] // CRLF is one line ending
			}
			f.pending = append(f.pending, '\r')
			return
		default:
			f.pending = append(f.pending, c)
		}
	}
}
