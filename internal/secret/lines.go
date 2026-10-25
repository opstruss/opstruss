package secret

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/opstruss/opstruss/internal/ansi"
)

// MaxLine is the longest line of output that one log event holds; a longer
// line goes in pieces.
const MaxLine = 64 << 10

// Lines turns what a program writes to one stream into lines for emit, as
// a Stream hides them and ansi.Clean cleans them, in bounded memory: a
// line longer than MaxLine goes in pieces, cut where no value, shape, or
// key line that the Stream hides is cut in two.
type Lines struct {
	s    *Stream
	emit func(string)
	buf  []byte
}

// NewLines returns Lines that hide with s, or hide nothing when s is nil.
func NewLines(s *Stream, emit func(string)) *Lines { return &Lines{s: s, emit: emit} }

// Write takes what the stream holds next, at most MaxLine of an unended
// line at a time.
func (l *Lines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		room := MaxLine - len(l.buf)
		if room <= 0 {
			// What may be the start of a long hidden value waits for more.
			room = MaxLine
		}
		k := min(len(p), room)
		l.buf = append(l.buf, p[:k]...)
		p = p[k:]
		for {
			i := bytes.IndexByte(l.buf, '\n')
			if i < 0 {
				break
			}
			l.line(string(l.buf[:i]))
			l.buf = l.buf[i+1:]
		}
		if len(l.buf) >= MaxLine {
			out, keep := l.cut(string(l.buf))
			if out != "" {
				l.out(out)
			}
			l.buf = append(l.buf[:0], keep...)
		}
	}
	return n, nil
}

// Flush hands on the last line, which no newline ended.
func (l *Lines) Flush() {
	if len(l.buf) > 0 {
		l.line(string(l.buf))
		l.buf = l.buf[:0]
	}
}

func (l *Lines) line(text string) {
	if l.s != nil {
		text = l.s.Line(text)
	}
	l.out(text)
}

// out hands on a line, or a piece of one, that the Stream hid, as
// ansi.Clean cleans it. A value or a shape that color codes cut, which the
// bytes as they are do not show, is looked for in the text without them,
// and when one is there, the line goes without color, hidden.
func (l *Lines) out(text string) {
	text = strings.ToValidUTF8(text, "�")
	clean := ansi.Clean(text)
	if l.s != nil && (clean != text || strings.IndexByte(clean, 0x1b) >= 0) {
		plain := ansi.Strip(clean)
		if hidden := l.s.r.Log(plain); hidden != plain {
			clean = hidden
		}
	}
	l.emit(clean)
}

// cut splits the start of a long line into what goes out now and what is
// kept for the rest of it; without a Stream, only a character is not cut.
func (l *Lines) cut(text string) (out, keep string) {
	if l.s != nil {
		return l.s.Cut(text)
	}
	i := len(text)
	for start := i - 1; start >= 0 && start > i-utf8.UTFMax; start-- {
		if utf8.RuneStart(text[start]) {
			if !utf8.FullRuneInString(text[start:]) {
				i = start
			}
			break
		}
	}
	return text[:i], text[i:]
}
