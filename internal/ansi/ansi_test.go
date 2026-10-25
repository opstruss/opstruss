package ansi

import "testing"

func TestClean(t *testing.T) {
	for _, c := range []struct{ in, clean, strip string }{
		{"plain text\twith a tab", "plain text\twith a tab", "plain text\twith a tab"},
		{"\x1b[1;31mFAIL\x1b[0m nothing", "\x1b[1;31mFAIL\x1b[0m nothing", "FAIL nothing"},
		{"\x1b[38;5;196mred\x1b[m", "\x1b[38;5;196mred\x1b[m", "red"},
		{"\x1b[38:2::255:0:0mtrue\x1b[39m", "\x1b[38:2::255:0:0mtrue\x1b[39m", "true"},
		{"a\x1b[2Kb\x1b[1Ac\x1b[?25ld", "abcd", "abcd"},
		{"x\x1b]0;title\x07y", "xy", "xy"},
		{"x\x1b]52;c;Zm9v\x1b\\y", "xy", "xy"},
		{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "link", "link"},
		{"a\x1bP1$r0m\x1b\\b", "ab", "ab"},
		{"a\x1bcb\x1b7c\x1b(Bd", "abcd", "abcd"},
		{"bell\x07 back\bspace\x7f", "bell backspace", "bell backspace"},
		{"a\rb", "b", "b"},
		{"crlf\r", "crlf", "crlf"},
		{"10%\r50%\r100%\r", "100%", "100%"},
		{"\x1b[32m50%\r100%", "100%", "100%"},
		{"c1 \u009b31m csi", "c1 31m csi", "c1 31m csi"},
		{"cut \x1b[31", "cut ", "cut "},
		{"cut \x1b]0;tit", "cut ", "cut "},
		{"cut \x1b", "cut ", "cut "},
		{"first\x1b[1A\nsecond\rSECOND", "first\nSECOND", "first\nSECOND"},
		{"é ü 日本", "é ü 日本", "é ü 日本"},
	} {
		if got := Clean(c.in); got != c.clean {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.clean)
		}
		if got := Strip(c.in); got != c.strip {
			t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.strip)
		}
	}
}

// Text with nothing to drop comes back as it is, without a copy.
func TestCleanPlain(t *testing.T) {
	s := "a line of output, with no escape\tat all"
	if n := testing.AllocsPerRun(100, func() { _ = Clean(s); _ = Strip(s) }); n != 0 {
		t.Fatalf("%v allocations", n)
	}
}

func TestColor(t *testing.T) {
	for _, c := range []struct {
		env      map[string]string
		terminal bool
		color    bool
		forced   bool
	}{
		{nil, true, true, false},
		{nil, false, false, false},
		{map[string]string{"NO_COLOR": "1"}, true, false, false},
		{map[string]string{"NO_COLOR": ""}, true, true, false},
		{map[string]string{"FORCE_COLOR": "1"}, false, true, true},
		{map[string]string{"FORCE_COLOR": "0"}, false, false, false},
		{map[string]string{"CLICOLOR_FORCE": "1"}, false, true, true},
		{map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, false, false, false},
		{map[string]string{"TERM": "dumb"}, true, false, false},
		{map[string]string{"TERM": "dumb", "FORCE_COLOR": "1"}, true, true, true},
		{map[string]string{"GITHUB_ACTIONS": "true"}, false, true, false},
		{map[string]string{"GITHUB_ACTIONS": "true", "NO_COLOR": "1"}, false, false, false},
	} {
		get := func(k string) string { return c.env[k] }
		if got := Color(get, c.terminal); got != c.color {
			t.Errorf("Color(%v, %v) = %v", c.env, c.terminal, got)
		}
		if on, _ := Forced(get); on != c.forced {
			t.Errorf("Forced(%v) = %v", c.env, on)
		}
	}
}
