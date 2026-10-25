package secret

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A line without an end goes out in pieces of about MaxLine as it comes,
// whatever the size of each write, and a known value or a shape cut by a
// piece is hidden whole.
func TestLines(t *testing.T) {
	red := NewRedactor()
	red.Add("KNOWN-SECRET-CANARY")
	shape := "gh" + "p_" + strings.Repeat("aB3dE5", 7)
	for _, v := range []string{"KNOWN-SECRET-CANARY", shape} {
		for _, at := range []int{1, 5, len(v) - 1} {
			text := strings.Repeat("x", MaxLine-at-1) + " " + v + " " + strings.Repeat("y", 3*MaxLine) + "\nend"
			for _, size := range []int{1, 4096, len(text)} {
				var got []string
				l := NewLines(red.Stream(), func(s string) { got = append(got, s) })
				for i := 0; i < len(text); i += size {
					_, _ = l.Write([]byte(text[i:min(i+size, len(text))]))
					if len(l.buf) > MaxLine+maxShape {
						t.Fatalf("%s at %d by %d: holds %d bytes", v, at, size, len(l.buf))
					}
				}
				l.Flush()
				all := strings.Join(got, "")
				if strings.Contains(all, v) || !strings.Contains(all, Mask) || got[len(got)-1] != "end" {
					t.Fatalf("%s at %d by %d: %d pieces, last %q", v, at, size, len(got), got[len(got)-1])
				}
				for _, piece := range got {
					if len(piece) > MaxLine+maxShape {
						t.Fatalf("a piece of %d bytes", len(piece))
					}
				}
			}
		}
	}
}

// Without a Stream nothing is hidden, and no character is cut in two.
func TestLinesPlain(t *testing.T) {
	text := strings.Repeat("é", MaxLine) + "\n"
	var got []string
	l := NewLines(nil, func(s string) { got = append(got, s) })
	_, _ = l.Write([]byte(text))
	l.Flush()
	if len(got) < 2 || strings.Join(got, "") != strings.TrimSuffix(text, "\n") {
		t.Fatalf("%d pieces", len(got))
	}
	for _, piece := range got {
		if !utf8.ValidString(piece) || strings.ContainsRune(piece, utf8.RuneError) {
			t.Fatal("a character is cut")
		}
	}
}

// Lines go out clean: color stays, other escapes go, and a carriage
// return starts the line again. A value or a shape that color codes cut
// is hidden too, in a line that loses its color.
func TestLinesClean(t *testing.T) {
	red := NewRedactor()
	red.Add("KNOWN-SECRET-CANARY")
	shape := "gh" + "p_" + strings.Repeat("aB3dE5", 7)
	for _, c := range []struct{ in, want string }{
		{"\x1b[32mok\x1b[0m\n", "\x1b[32mok\x1b[0m"},
		{"x\x1b]0;title\x07y\n", "xy"},
		{"10%\r50%\r100%\r\n", "100%"},
		{"\x1b[32mKNOWN-SECRET-CANARY\x1b[0m\n", "\x1b[32m" + Mask + "\x1b[0m"},
		{"key KNOWN-\x1b[1mSECRET\x1b[0m-CANARY end\n", "key " + Mask + " end"},
		{"token " + shape[:9] + "\x1b[33m" + shape[9:] + "\x1b[0m\n", "token " + Mask},
		{"moved KNOWN-SECRET\x1b[2K-CANARY\n", "moved " + Mask},
	} {
		var got []string
		l := NewLines(red.Stream(), func(s string) { got = append(got, s) })
		_, _ = l.Write([]byte(c.in))
		l.Flush()
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
