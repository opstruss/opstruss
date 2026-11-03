package ansi

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// draw writes each piece to a new screen and returns the rows that left its
// top and those left at the end.
func draw(cols, rows int, pieces ...string) (gone, left []string) {
	s := NewScreen(cols, rows)
	for _, p := range pieces {
		gone = append(gone, s.Write([]byte(p))...)
	}
	return gone, s.End()
}

func TestScreen(t *testing.T) {
	for _, c := range []struct {
		name       string
		cols, rows int
		in         string
		gone, left []string
	}{
		{"text", 10, 3, "ab\r\ncd\r\n", nil, []string{"ab", "cd"}},
		{"progress", 10, 3, "0%\r50%\r100%\r\n", nil, []string{"100%"}},
		{"backspace", 10, 3, "ab\bc", nil, []string{"ac"}},
		{"spinner", 10, 3, "⠋\b⠙\b⠹ go", nil, []string{"⠹ go"}},
		{"tab", 20, 3, "a\tb\tc", nil, []string{"a       b       c"}},
		{"lf keeps the column", 10, 3, "ab\ncd", nil, []string{"ab", "  cd"}},
		{"vt and ff", 10, 3, "a\vb\fc", nil, []string{"a", " b", "  c"}},
		{"other c0 and del go", 10, 3, "a\x00\x07\x7fb", nil, []string{"ab"}},
		{"c1 goes", 10, 3, "a\u009b31mb", nil, []string{"a31mb"}},
		{"scroll", 10, 2, "1\r\n2\r\n3\r\n4", []string{"1", "2"}, []string{"3", "4"}},
		{"blank rows inside stay", 10, 4, "a\r\n\r\nb", nil, []string{"a", "", "b"}},
		{"autowrap", 4, 3, "abcdef", nil, []string{"abcd", "ef"}},
		{"pending wrap and crlf", 4, 3, "abcd\r\nef", nil, []string{"abcd", "ef"}},
		{"wrap scrolls", 3, 2, "abcdefgh", []string{"abc"}, []string{"def", "gh"}},
		{"wide", 6, 3, "日本x", nil, []string{"日本x"}},
		{"wide at the last column", 5, 3, "abcd日", nil, []string{"abcd", "日"}},
		{"wide split", 6, 3, "日本\x1b[2Gx", nil, []string{" x本"}},
		{"combining", 6, 3, "éx", nil, []string{"éx"}},
		{"zwj", 6, 3, "👩‍💻", nil, []string{"👩‍💻"}},
		{"mark at the start goes", 6, 3, "́x", nil, []string{"x"}},
		{"cuu and cha", 10, 3, "aaa\r\nbbb\r\n\x1b[2A\x1b[2Gx", nil, []string{"axa", "bbb"}},
		{"cud cuf cub", 10, 3, "\x1b[2B\x1b[3Cx\x1b[2Dy", nil, []string{"", "", "  yx"}},
		{"cnl cpl", 10, 3, "ab\x1b[Ecd\x1b[Fx", nil, []string{"xb", "cd"}},
		{"cup", 10, 3, "\x1b[2;3Hx\x1b[Hy\x1b[3;1fz", nil, []string{"y", "  x", "z"}},
		{"vpa", 10, 3, "a\x1b[3dz", nil, []string{"a", "", " z"}},
		{"moves stop at the edges", 4, 2, "\x1b[9A\x1b[9Da\x1b[9C\x1b[9Bb", nil, []string{"a", "   b"}},
		{"cha 0 is 1", 10, 3, "abc\x1b[0Gx", nil, []string{"xbc"}},
		{"el 0", 10, 3, "abcdef\x1b[3G\x1b[K", nil, []string{"ab"}},
		{"el 1", 10, 3, "abcdef\x1b[3G\x1b[1K", nil, []string{"   def"}},
		{"el 2", 10, 3, "abcdef\x1b[2K", nil, nil},
		{"ed 0", 10, 3, "aaa\r\nbbb\r\nccc\x1b[2;2H\x1b[J", nil, []string{"aaa", "b"}},
		{"ed 1", 10, 3, "aaa\r\nbbb\r\nccc\x1b[2;2H\x1b[1J", nil, []string{"", "  b", "ccc"}},
		{"ed 2 leaves no rows", 10, 3, "aaa\r\nbbb\x1b[2Jx", nil, []string{"", "   x"}},
		{"ed 3", 10, 3, "aaa\x1b[3J", nil, nil},
		{"il", 10, 3, "a\r\nb\r\nc\x1b[2;3H\x1b[L", nil, []string{"a", "", "b"}},
		{"dl", 10, 3, "a\r\nb\r\nc\x1b[1;3H\x1b[M", nil, []string{"b", "c"}},
		{"dl at the top is no scrollback", 10, 2, "a\r\nb\x1b[H\x1b[2M", nil, nil},
		{"ich", 10, 3, "abc\x1b[2G\x1b[2@", nil, []string{"a  bc"}},
		{"dch", 10, 3, "abcdef\x1b[2G\x1b[2P", nil, []string{"adef"}},
		{"ech", 10, 3, "abcdef\x1b[2G\x1b[2X", nil, []string{"a  def"}},
		{"su", 10, 3, "a\r\nb\r\nc\x1b[S", []string{"a"}, []string{"b", "c"}},
		{"sd", 10, 3, "a\r\nb\r\nc\x1b[T", nil, []string{"", "a", "b"}},
		{"region below the top drops rows", 10, 3, "top\x1b[2;3r\x1b[3;1Hx\r\ny\r\nz", nil, []string{"top", "y", "z"}},
		{"ri", 10, 3, "a\r\nb\x1b[H\x1bMz", nil, []string{"z", "a", "b"}},
		{"ri moves up", 10, 3, "a\r\nb\x1bMz", nil, []string{"az", "b"}},
		{"ind and nel", 10, 3, "a\x1bDb\x1bEc", nil, []string{"a", " b", "c"}},
		{"decsc decrc", 10, 3, "\x1b[31ma\x1b7\x1b[0m\r\nb\x1b8c", nil, []string{"\x1b[0;31mac\x1b[0m", "b"}},
		{"csi s u", 10, 3, "ab\x1b[s\r\ncd\x1b[ux", nil, []string{"abx", "cd"}},
		{"restore without save", 10, 3, "\x1b[31mab\x1b8c", nil, []string{"c\x1b[0;31mb\x1b[0m"}},
		{"ris", 10, 3, "\x1b[31mab\r\ncd\x1bcx", nil, []string{"x"}},
		{"private and unknown go", 10, 3, "\x1b[?25la\x1b[?1049hb\x1b[0 qc\x1b[5ne\x1b(Bf\x1b=g", nil, []string{"abcefg"}},
		{"strings go", 10, 3, "a\x1b]0;title\x07b\x1b]52;c;Zm9v\x1b\\c\x1bP1$r\x1b\\d", nil, []string{"abcd"}},
		{"cut csi", 10, 3, "a\x1b[3\x01b", nil, []string{"ab"}},
	} {
		gone, left := draw(c.cols, c.rows, c.in)
		if !slices.Equal(gone, c.gone) || !slices.Equal(left, c.left) {
			t.Errorf("%s: %q gives gone %q, left %q; want %q, %q", c.name, c.in, gone, left, c.gone, c.left)
		}
	}
}

func TestScreenStyle(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"\x1b[32m✔\x1b[0m done", "\x1b[0;32m✔\x1b[0m done"},
		{"\x1b[1;31mx", "\x1b[0;1;31mx\x1b[0m"},
		{"\x1b[01;031mx\x1b[m", "\x1b[0;1;31mx\x1b[0m"},
		{"\x1b[31m\x1b[1mx\x1b[22my\x1b[39mz", "\x1b[0;1;31mx\x1b[0;31my\x1b[0mz"},
		{"\x1b[91;104mx", "\x1b[0;91;104mx\x1b[0m"},
		{"\x1b[38;5;196mx\x1b[38;5;3my", "\x1b[0;38;5;196mx\x1b[0;33my\x1b[0m"},
		{"\x1b[38;2;255;0;10mx", "\x1b[0;38;2;255;0;10mx\x1b[0m"},
		{"\x1b[38:2::255:0:10mx\x1b[48:5:17my", "\x1b[0;38;2;255;0;10mx\x1b[0;38;2;255;0;10;48;5;17my\x1b[0m"},
		{"\x1b[38:2:1:2:3mx", "\x1b[0;38;2;1;2;3mx\x1b[0m"},
		{"\x1b[38;5;300mx\x1b[38;2;1mz", "xz"},
		{"\x1b[2;3;4;5;7;8;9mx\x1b[23;24;25;27;28;29my", "\x1b[0;2;3;4;5;7;8;9mx\x1b[0;2my\x1b[0m"},
		{"\x1b[4:3mx\x1b[4:0my\x1b[21mz", "\x1b[0;4mx\x1b[0my\x1b[0;4mz\x1b[0m"},
		{"\x1b[6mx\x1b[26;99my", "\x1b[0;5mxy\x1b[0m"},
		{"\x1b[;1mx", "\x1b[0;1mx\x1b[0m"},
		{"\x1b[44m  \x1b[0m", "\x1b[0;44m  \x1b[0m"},
		{"\x1b[7m \x1b[0m  ", "\x1b[0;7m \x1b[0m"},
		{"x  \x1b[4m  ", "x"},
		{"x\x1b[44m\x1b[K", "x\x1b[0;44m         \x1b[0m"},
	} {
		if _, left := draw(10, 2, c.in); len(left) != 1 && c.want != "" || len(left) == 1 && left[0] != c.want {
			t.Errorf("%q gives %q, want %q", c.in, left, c.want)
		}
	}
}

// TestScreenCompose draws what docker compose wrote to a terminal of 88 × 30
// as it brought three services up and down: what is left is the screen that
// terminal showed, in color, cut anywhere.
func TestScreenCompose(t *testing.T) {
	for _, name := range []string{"up", "down"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			composeScreen(t, name)
		})
	}
}

func composeScreen(t *testing.T, name string) {
	raw, err := os.ReadFile("testdata/compose-" + name + "-pty-88x30.raw")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/compose-" + name + "-pty-88x30.screen.txt")
	if err != nil {
		t.Fatal(err)
	}
	gone, rows := draw(88, 30, string(raw))
	if len(gone) > 0 {
		t.Errorf("%s: rows left the top: %q", name, gone)
	}
	var plain []string
	for _, r := range rows {
		plain = append(plain, Strip(r))
	}
	if got := strings.Join(plain, "\n") + "\n"; got != string(want) {
		t.Errorf("%s: screen\n%s\nwant\n%s", name, got, want)
	}
	if !strings.HasPrefix(rows[1], " \x1b[0;32m✔\x1b[0m ") {
		t.Errorf("%s: %q has no green ✔", name, rows[1])
	}
	for at := range raw {
		gone, cut := draw(88, 30, string(raw[:at]), string(raw[at:]))
		if len(gone) > 0 || !slices.Equal(cut, rows) {
			t.Fatalf("%s cut at %d: gone %q, left %q", name, at, gone, cut)
		}
	}
}

// TestScreenRegion draws as apt does: lines scroll in a region above a bar
// in the last row, which goes once the region is the whole screen again.
func TestScreenRegion(t *testing.T) {
	s := NewScreen(20, 4)
	var gone []string
	gone = append(gone, s.Write([]byte("\x1b7\x1b[1;3r\x1b8"))...)
	for _, l := range []string{"get 1", "get 2", "get 3", "get 4", "get 5"} {
		gone = append(gone, s.Write([]byte(l+"\r\n\x1b7\x1b[4;1H\x1b[44m[##  ]\x1b[0m\x1b8"))...)
	}
	if want := []string{"get 1", "get 2", "get 3"}; !slices.Equal(gone, want) {
		t.Errorf("gone %q, want %q", gone, want)
	}
	if rows := s.Rows(); len(rows) != 4 || rows[3] != "\x1b[0;44m[##  ]\x1b[0m" {
		t.Errorf("rows %q", rows)
	}
	gone = s.Write([]byte("\x1b7\x1b[r\x1b8\x1b[4;1H\x1b[J"))
	if rows := s.End(); len(gone) > 0 || !slices.Equal(rows, []string{"get 4", "get 5"}) {
		t.Errorf("gone %q, rows %q", gone, rows)
	}
}

func TestScanner(t *testing.T) {
	for _, c := range []struct {
		pieces []string
		want   string // T(text) and C(control) in order
	}{
		{[]string{"ab\x1b[31mcd\r\n"}, "T(ab)C(\x1b[31m)T(cd)C(\r)C(\n)"},
		{[]string{"a\tb"}, "T(a\tb)"},
		{[]string{"ab\x1b[3", "1mcd"}, "T(ab)C(\x1b[31m)T(cd)"},
		{[]string{"ab\x1b", "[31m"}, "T(ab)C(\x1b[31m)"},
		{[]string{"é\xc3", "\xa9"}, "T(é)T(é)"},
		{[]string{"a\xc2", "\x9bb"}, "T(a)C(\u009b)T(b)"},
		{[]string{"a\x1b]0;ti", "tle\x07b"}, "T(a)T(b)"},
		{[]string{"a\x1b]0;ti\x1b", "\\b"}, "T(a)T(b)"},
		{[]string{"a\x1b]0;", "t\x1b", "x\x1b", "\\b"}, "T(a)T(b)"},
		{[]string{"a\x1b]0;t\x1b]x\x07b"}, "T(a)T(b)"},
		{[]string{"a\x1b[" + strings.Repeat("1;", maxRest), "m"}, "T(a)T(m)"},
		{[]string{"\x80x"}, "T(\x80x)"},
		{[]string{"", "a"}, "T(a)"},
	} {
		var sc Scanner
		var b strings.Builder
		for _, p := range c.pieces {
			sc.Scan([]byte(p), func(s string) { b.WriteString("T(" + s + ")") }, func(s string) { b.WriteString("C(" + s + ")") })
		}
		if b.String() != c.want {
			t.Errorf("%q gives %q, want %q", c.pieces, b.String(), c.want)
		}
	}
}

// A long string sequence costs no memory, and a long run of marks stays in
// its cell.
func TestScreenBounded(t *testing.T) {
	s := NewScreen(10, 2)
	s.Write([]byte("a\x1b]52;c;"))
	for range 100 {
		s.Write([]byte(strings.Repeat("Zm9v", 1<<10)))
	}
	s.Write([]byte("\x07b" + "e" + strings.Repeat("́", 1<<12)))
	if len(s.scan.rest) > 0 || len(s.grid[0][2].text) > maxCell {
		t.Errorf("rest %d bytes, cell %d bytes", len(s.scan.rest), len(s.grid[0][2].text))
	}
	if rows := s.End(); len(rows) != 1 || !strings.HasPrefix(rows[0], "abé") {
		t.Errorf("rows %q", rows)
	}
}

func FuzzScreen(f *testing.F) {
	raw, err := os.ReadFile("testdata/compose-up-pty-88x30.raw")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw[:2000], 7)
	f.Add([]byte("\x1b[2;3r\x1b[3;1Hx\r\ny\x1b[L\x1b[2@日́\x1b[38:2::1:2:3m\x1b]0;t"), 3)
	f.Fuzz(func(t *testing.T, data []byte, at int) {
		s := NewScreen(9, 4)
		at = min(max(at, 0), len(data))
		rows := append(s.Write(data[:at]), s.Write(data[at:])...)
		rows = append(append(rows, s.Rows()...), s.End()...)
		for _, r := range rows {
			if Clean(r) != r {
				t.Fatalf("row %q is not as Clean leaves it: %q", r, Clean(r))
			}
		}
	})
}
