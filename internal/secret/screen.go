package secret

import "github.com/opstruss/opstruss/internal/ansi"

// Screen turns what a program writes to a terminal of its own into the rows
// that the terminal shows, hidden as Lines hides a stream. Each run of text
// between controls is hidden before the terminal draws it, as a line is,
// and each row the terminal shows is looked at again, since moves of the
// cursor can put a value together that no run held. A run that no control
// ends yet waits, as much of it as Stream.Cut keeps, so a value is not cut
// in two; what a write ends with, such as progress after \r, shows once
// the next control comes.
type Screen struct {
	s      *Stream
	emit   func(string)
	screen *ansi.Screen
	scan   ansi.Scanner
	run    string // the text since the last control
	buf    []byte // what the terminal draws of the current write
}

// NewScreen returns a Screen that turns what a program writes to its
// terminal into the rows that the terminal shows, hidden by s (nothing when
// s is nil), as Lines does for a stream; emit gets each row that leaves the
// top of the screen.
func NewScreen(s *Stream, cols, rows int, emit func(string)) *Screen {
	return &Screen{s: s, emit: emit, screen: ansi.NewScreen(cols, rows)}
}

func (sc *Screen) Write(p []byte) (int, error) {
	sc.buf = sc.buf[:0]
	sc.scan.Scan(p, sc.text, sc.control)
	if sc.run != "" {
		out := sc.run
		sc.run = ""
		if sc.s != nil {
			out, sc.run = sc.s.Cut(out)
		}
		sc.buf = append(sc.buf, out...)
	}
	sc.out(sc.screen.Write(sc.buf))
	return len(p), nil
}

func (sc *Screen) text(t string) { sc.run += t }

func (sc *Screen) control(c string) {
	sc.buf = append(append(sc.buf, sc.line()...), c...)
}

// line returns the run that a control ended, hidden.
func (sc *Screen) line() string {
	run := sc.run
	sc.run = ""
	if sc.s != nil && run != "" {
		run = sc.s.Line(run)
	}
	return run
}

// Rows returns the rows that the program may still change, hidden.
func (sc *Screen) Rows() []string {
	rows := sc.screen.Rows()
	for i, r := range rows {
		rows[i] = sc.check(r)
	}
	return rows
}

// Flush emits the rows left once the program ended.
func (sc *Screen) Flush() {
	sc.out(sc.screen.Write([]byte(sc.line())))
	sc.out(sc.screen.End())
}

func (sc *Screen) out(rows []string) {
	for _, r := range rows {
		sc.emit(sc.check(r))
	}
}

func (sc *Screen) check(row string) string {
	if sc.s == nil {
		return row
	}
	return sc.s.recheck(row)
}
