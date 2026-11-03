package ansi

import (
	"slices"
	"testing"
)

func TestWrap(t *testing.T) {
	for _, c := range []struct {
		in            string
		indent, width int
		want          []string
	}{
		{"short", 2, 10, []string{"short"}},
		{"exactly8", 2, 10, []string{"exactly8"}},
		{"abcdefghij", 2, 10, []string{"abcdefgh", "ij"}},
		{"abcdefghij", 0, 0, []string{"abcdefghij"}},
		{"abcdefghij", 10, 10, []string{"abcdefghij"}},
		{"\x1b[31mabcdefghij\x1b[0m", 2, 6, []string{"\x1b[31mabcd\x1b[0m", "\x1b[31mefgh\x1b[0m", "\x1b[31mij\x1b[0m"}},
		{"\x1b[1m\x1b[31mabcd\x1b[0mefgh", 2, 6, []string{"\x1b[1m\x1b[31mabcd\x1b[0m", "efgh"}},
		{"ab\x1b[32mcdef\x1b[mg", 2, 6, []string{"ab\x1b[32mcd\x1b[0m", "\x1b[32mef\x1b[mg"}},
		{"日本語です", 2, 7, []string{"日本", "語で", "す"}},
		{"e\u0301e\u0301e\u0301e\u0301", 2, 4, []string{"e\u0301e\u0301", "e\u0301e\u0301"}},
		{"a\tb", 2, 20, []string{"a\tb"}},
		{"a\tbcdefghijklmn", 2, 12, []string{"a     bcde", "fghijklmn"}},
		{"日", 2, 3, []string{"日"}},
	} {
		if got := Wrap(c.in, c.indent, c.width); !slices.Equal(got, c.want) {
			t.Errorf("Wrap(%q, %d, %d) = %q, want %q", c.in, c.indent, c.width, got, c.want)
		}
	}
}
