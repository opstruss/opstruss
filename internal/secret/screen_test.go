package secret

import (
	"slices"
	"strings"
	"testing"
)

// screen writes each piece to a Screen 40 × 5 that red hides with, and
// returns the rows that it emitted, the rows it shows after each piece, and
// those that Flush emits.
func screen(red *Redactor, pieces ...string) (emitted []string, shown [][]string, flushed []string) {
	sc := NewScreen(red.Stream(), 40, 5, func(r string) { emitted = append(emitted, r) })
	for _, p := range pieces {
		_, _ = sc.Write([]byte(p))
		shown = append(shown, sc.Rows())
	}
	n := len(emitted)
	sc.Flush()
	return emitted[:n], shown, emitted[n:]
}

func TestScreen(t *testing.T) {
	red := NewRedactor()
	red.Add("tok123")
	ghp := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	for _, c := range []struct {
		name    string
		pieces  []string
		shown   [][]string
		flushed []string
	}{
		{"a value that two writes cut", []string{"x tok", "123 y\r\n"}, [][]string{{}, {"x *** y"}}, []string{"x *** y"}},
		{"a value that moves of the cursor put together", []string{"\x1b[4G123\x1b[1G\x1b[32mtok\x1b[0m\r\n"}, [][]string{{"***"}}, []string{"***"}},
		{"a shape, as it shows", []string{"login " + ghp + "\r"}, [][]string{{"login ***"}}, []string{"login ***"}},
		{"progress shows at the next control", []string{"\r10%", "\r50%", "\r100%\r\n"}, [][]string{{}, {"10%"}, {"100%"}}, []string{"100%"}},
		{"a spinner shows at once", []string{"⠋\b", "⠙\b"}, [][]string{{"⠋"}, {"⠙"}}, []string{"⠙"}},
		{"a run that no control ends waits", []string{"almost"}, [][]string{{}}, []string{"almost"}},
	} {
		_, shown, flushed := screen(red, c.pieces...)
		for i := range shown {
			if shown[i] == nil {
				shown[i] = []string{}
			}
		}
		if !slices.EqualFunc(shown, c.shown, slices.Equal) || !slices.Equal(flushed, c.flushed) {
			t.Errorf("%s: shows %q, flushes %q; want %q, %q", c.name, shown, flushed, c.shown, c.flushed)
		}
	}
}

// Each line of a private key is hidden, also as rows that leave the top.
func TestScreenKey(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\r\nb3BlbnNzaC1rZXktdjEAAAAA\r\nQyNTUxOQAAACB\r\n-----END OPENSSH PRIVATE KEY-----\r\nafter\r\n"
	emitted, _, flushed := screen(NewRedactor(), key, "1\r\n2\r\n3\r\n4\r\n")
	for _, r := range append(emitted, flushed...) {
		if strings.Contains(r, "b3Bl") || strings.Contains(r, "QyNT") || strings.Contains(r, "-----") {
			t.Fatalf("row %q shows the key; rows %q, %q", r, emitted, flushed)
		}
	}
	if want := []string{Mask, Mask, Mask, Mask, "after"}; !slices.Equal(emitted, want) {
		t.Errorf("emitted %q, want %q", emitted, want)
	}
}

// A long run without a control keeps no more than Cut keeps.
func TestScreenBounded(t *testing.T) {
	red := NewRedactor()
	sc := NewScreen(red.Stream(), 40, 5, func(string) {})
	for range 64 {
		_, _ = sc.Write([]byte(strings.Repeat("y", 1<<14)))
		if len(sc.run) > maxShape {
			t.Fatalf("holds %d bytes", len(sc.run))
		}
	}
}

// Without a Stream nothing is hidden and nothing waits.
func TestScreenPlain(t *testing.T) {
	var got []string
	sc := NewScreen(nil, 40, 5, func(r string) { got = append(got, r) })
	_, _ = sc.Write([]byte("tok123 50%"))
	if rows := sc.Rows(); !slices.Equal(rows, []string{"tok123 50%"}) {
		t.Errorf("rows %q", rows)
	}
	sc.Flush()
	if !slices.Equal(got, []string{"tok123 50%"}) {
		t.Errorf("flushed %q", got)
	}
}
