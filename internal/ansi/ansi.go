// Package ansi cleans what programs print for the readers of a run: color
// stays, and every other escape sequence and control character goes, so
// the output of a program cannot move the cursor, clear the screen, or set
// the title or the clipboard of the terminal that shows it, as less -R
// lets only color through.
package ansi

import "strings"

const esc = 0x1b

// Clean returns s with its SGR sequences (ESC [ … m), which set color and
// style, its tabs, and its newlines; every other escape sequence and
// control character is dropped. In each line, a carriage return starts the
// line again, as a progress bar does: the text after the last one is the
// line, once those that end it, as in CRLF, are dropped.
func Clean(s string) string { return clean(s, true) }

// Strip is Clean without the SGR sequences too: the text alone.
func Strip(s string) string { return clean(s, false) }

func clean(s string, sgr bool) string {
	if plain(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		line = strings.TrimRight(line, "\r")
		if j := strings.LastIndexByte(line, '\r'); j >= 0 {
			line = line[j+1:]
		}
		cleanLine(&b, line, sgr)
	}
	return b.String()
}

// plain reports whether s has nothing to look at: no control character but
// tab and newline, which leaves out escapes too, no DEL, and no C1 control.
func plain(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\t' || c == '\n':
		case c < 0x20 || c == 0x7f:
			return false
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			return false
		}
	}
	return true
}

func cleanLine(b *strings.Builder, s string, sgr bool) {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == esc:
			n, keep := sequence(s[i:])
			if keep && sgr {
				b.WriteString(s[i : i+n])
			}
			i += n
		case c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			// A C1 control, such as the 8-bit CSI, which some terminals obey.
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}
}

// sequence measures the escape sequence at the start of s: its length, and
// whether it is SGR. A sequence that s cuts off takes the rest of s.
func sequence(s string) (n int, sgr bool) {
	if len(s) < 2 {
		return len(s), false
	}
	switch s[1] {
	case '[':
		// CSI: parameter bytes, intermediate bytes, then a final byte.
		i := 2
		for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
			i++
		}
		params := s[2:i]
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i >= len(s) || s[i] < 0x40 || s[i] > 0x7e {
			if i < len(s) {
				// A byte that cannot be in a CSI ends it, and stays text.
				return i, false
			}
			return len(s), false
		}
		return i + 1, s[i] == 'm' && i == 2+len(params) && strings.Trim(params, "0123456789;:") == ""
	case ']', 'P', 'X', '^', '_':
		// OSC, DCS, SOS, PM, APC: a string up to ST (ESC \) or BEL.
		for i := 2; i < len(s); i++ {
			switch {
			case s[i] == 0x07:
				return i + 1, false
			case s[i] == esc && i+1 < len(s) && s[i+1] == '\\':
				return i + 2, false
			}
		}
		return len(s), false
	}
	// Two bytes, or intermediate bytes and a final byte, as ESC ( B.
	i := 1
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) {
		i++
	}
	return i, false
}
