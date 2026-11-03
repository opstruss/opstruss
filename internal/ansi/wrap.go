package ansi

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Wrap splits s, a line as Clean leaves it, into the rows that a terminal
// width columns wide shows it in when each row starts at column indent,
// so that the caller can lead each row, as the first, with a gutter that
// the rows the terminal wraps itself would lack. A row that a color goes
// on past ends with a reset, and the next starts with that color again.
// The tabs of a line that does not fit become the spaces they stand for. A
// line that fits, or a width that leaves no room, comes back whole.
func Wrap(s string, indent, width int) []string {
	if width <= indent {
		return []string{s}
	}
	var rows []string
	var b strings.Builder
	var sgr []string // the SGR sequences in effect, since the last reset
	col, used := indent, false
	for i := 0; i < len(s); {
		if s[i] == esc {
			n, keep, _ := sequence(s[i:])
			seq := s[i : i+n]
			b.WriteString(seq)
			if keep {
				if strings.Trim(seq[2:n-1], "0") == "" {
					sgr = sgr[:0]
				} else {
					sgr = append(sgr, seq)
				}
			}
			i += n
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		w := runeWidth(r, col)
		if col+w > width && used {
			if len(sgr) > 0 {
				b.WriteString("\x1b[0m")
			}
			rows = append(rows, b.String())
			b.Reset()
			for _, q := range sgr {
				b.WriteString(q)
			}
			col = indent
			w = runeWidth(r, col)
		}
		if r == '\t' {
			b.WriteString(strings.Repeat(" ", w))
		} else {
			b.WriteString(s[i : i+n])
		}
		col += w
		used = true
		i += n
	}
	if rows == nil {
		return []string{s}
	}
	return append(rows, b.String())
}

// runeWidth is the columns that r takes at column col: up to the next tab
// stop for a tab, none for a mark that combines with the rune before it,
// and two for a wide rune, as of East Asian scripts and most emoji.
func runeWidth(r rune, col int) int {
	switch {
	case r == '\t':
		return 8 - col%8
	case r < 0x300 && r != 0xad:
		// Before the combining marks, only the soft hyphen has no width.
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}
