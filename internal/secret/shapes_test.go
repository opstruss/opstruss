package secret

import (
	"fmt"
	"strings"
	"testing"
)

// tok builds a canary of a credential's shape at run time, so that no
// scanner takes this file for one that holds a credential.
func tok(prefix string, n int) string {
	const alnum = "aB3dE5gH7jK9mN1pQ2sT4vW6yZ8cF0"
	var b strings.Builder
	b.WriteString(prefix)
	for i := range n {
		b.WriteByte(alnum[i%len(alnum)])
	}
	return b.String()
}

func shapeCases() (hidden, kept []string) {
	hidden = []string{
		tok("gh"+"p_", 36), tok("gh"+"o_", 36), tok("gh"+"u_", 36), tok("gh"+"s_", 36), tok("gh"+"r_", 36),
		tok("github"+"_pat_", 82), tok("gl"+"pat-", 20), tok("xo"+"xb-", 24), tok("xo"+"xp-", 40),
		tok("sk"+"_live_", 24), tok("rk"+"_live_", 99), tok("sk"+"-ant-", 93),
	}
	kept = []string{
		tok("gh"+"p_", 35), "ghp_short", tok("x_gh"+"p_", 36), tok("gl"+"pat-", 19), "xoxb-12", tok("sk"+"_test_", 24),
		tok("sk"+"_live_", 23), "sk-ant-short", "-----BEGIN PUBLIC KEY-----",
	}
	return hidden, kept
}

// Log hides the shapes of well-known credentials that no one told the run
// of, with the text around them; text alike that is shorter, or another
// kind of key, stays.
func TestLogShapes(t *testing.T) {
	r := NewRedactor()
	hidden, kept := shapeCases()
	for _, v := range hidden {
		for _, s := range []string{v, "token: " + v + " ok", `{"token":"` + v + `"}`, "https://" + v + "@example.com"} {
			got := r.Log(s)
			if strings.Contains(got, v) || !strings.Contains(got, Mask) {
				t.Errorf("Log(%q) = %q", s, got)
			}
		}
	}
	for _, v := range kept {
		if got := r.Log("value " + v); got != "value "+v {
			t.Errorf("Log(%q) = %q", v, got)
		}
	}
}

// A shape is hidden only in what the run writes out: it does not make a
// value known, so a value that holds it is not sensitive, and stays as it
// is in what the run hands on.
func TestShapesAreNotKnown(t *testing.T) {
	r := NewRedactor()
	r.Add("known-secret")
	v := tok("gh"+"p_", 36)
	out := map[string]any{"token": v, "list": []any{v}}
	if r.Contains(out) || r.String(v) != v || r.Value(out).(map[string]any)["token"] != v {
		t.Fatal("a shape counts as a known value")
	}
	if got := r.LogValue(out).(map[string]any); got["token"] != Mask || got["list"].([]any)[0] != Mask {
		t.Fatalf("LogValue %v", got)
	}
	r.AddDerived("prefix " + v)
	if r.String(v) != v {
		t.Fatal("a value derived from a shape became known")
	}
	if got := r.Err(fmt.Errorf("failed with %s and known-secret", v)).Error(); got != "failed with *** and ***" {
		t.Fatalf("Err %q", got)
	}
}

// A private key is hidden from its BEGIN line to its END line: across the
// lines of one stream, with its own state for each stream, and in one line
// where JSON writes its newlines as \n; one cut off hides the rest.
func TestStreamKeys(t *testing.T) {
	r := NewRedactor()
	key := []string{"-----BEGIN RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA0", "c2VjcmV0LWtleS1ib2R5", "-----END RSA PRIVATE KEY----- after"}
	out, other := r.Stream(), r.Stream()
	var got []string
	for i, line := range append([]string{"before"}, key...) {
		got = append(got, out.Line(line))
		if i == 2 {
			if s := other.Line("plain"); s != "plain" {
				t.Fatalf("the other stream wrote %q", s)
			}
		}
	}
	got = append(got, out.Line("next"))
	if want := []string{"before", Mask, Mask, Mask, Mask + " after", "next"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines %q", got)
	}
	for in, want := range map[string]string{
		`{"key":"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----\n","n":1}`: `{"key":"` + Mask + `\n","n":1}`,
		`key: -----BEGIN PRIVATE KEY-----\nMIIE cut off`:                                                            `key: ` + Mask,
		"-----BEGIN PGP PRIVATE KEY BLOCK----- x -----END PGP PRIVATE KEY BLOCK-----":                               Mask,
	} {
		if got := r.Log(in); got != want {
			t.Errorf("Log(%q) = %q, want %q", in, got, want)
		}
	}
	cut := r.Stream()
	cut.Line("-----BEGIN EC PRIVATE KEY-----")
	if got := cut.Line("MHcCAQEEI"); got != Mask {
		t.Fatalf("a key cut off at the end of its stream: %q", got)
	}
}

// A line too long to keep whole is cut where no known value, shape, or
// key line is cut in two.
func TestStreamCut(t *testing.T) {
	r := NewRedactor()
	r.Add("KNOWN-SECRET-CANARY")
	shape := tok("gh"+"p_", 40)
	for _, v := range []string{"KNOWN-SECRET-CANARY", shape, "-----BEGIN PRIVATE KEY-----"} {
		for at := 1; at < len(v); at++ {
			s := r.Stream()
			chunk := strings.Repeat("x", 4095-at) + " " + v[:at]
			out, keep := s.Cut(chunk)
			rest := keep + v[at:] + " tail"
			if strings.Contains(out, v[:min(len(v), 8)]) && strings.Contains(v, "KNOWN") {
				t.Fatalf("%s cut at %d: out ends %q", v, at, out[max(0, len(out)-30):])
			}
			line := out + s.Line(rest)
			if strings.Contains(line, v) || strings.Contains(out+rest, v) && !strings.Contains(line, Mask) {
				t.Fatalf("%s cut at %d leaked: %q", v, at, line[max(0, len(line)-80):])
			}
			if !strings.HasPrefix(line, strings.Repeat("x", 1000)) {
				t.Fatalf("%s cut at %d lost its start", v, at)
			}
		}
	}
	// Text without values is written out but for what may start one.
	out, keep := r.Stream().Cut(strings.Repeat("y", 4096))
	if len(out) == 0 || len(keep) > maxShape || out+keep != strings.Repeat("y", 4096) {
		t.Fatalf("cut %d and %d", len(out), len(keep))
	}
}

func TestHiddenName(t *testing.T) {
	for name, want := range map[string]bool{"GITHUB_TOKEN": true, "github_token": true, "DB_PASSWORD": true, "AWS_SECRET_ACCESS_KEY": true,
		"AWS_ACCESS_KEY": true, "APP_SECRET": true, "S3_SECRET_KEY": true, "TOKEN": false, "SSH_KEY": false, "PATH": false, "TOKEN_FILE": false} {
		if HiddenName(name) != want {
			t.Errorf("HiddenName(%s) is %v", name, !want)
		}
	}
}

// logLines are 10 MiB of output as a build writes it, one line in a hundred
// with a shape, the kind of log that a run hides values in.
func logLines(withShapes bool) []string {
	var lines []string
	size := 0
	for i := 0; size < 10<<20; i++ {
		line := fmt.Sprintf("2026-10-23T12:%02d:%02d.%06dZ info step %d: compiling github.com/example/pkg_%d/internal-%d (%d of 5000)", i/60%60, i%60, i, i, i%97, i%13, i)
		if withShapes && i%100 == 0 {
			line += " token=" + tok("gh"+"p_", 36)
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	return lines
}

func benchRedactor() *Redactor {
	r := NewRedactor()
	for i := range 10 {
		r.Add(fmt.Sprintf("secret-value-%02d-of-the-run", i))
	}
	return r
}

func benchLines(b *testing.B, lines []string, fn func(string) string) {
	b.SetBytes(10 << 20)
	b.ReportAllocs()
	for b.Loop() {
		for _, l := range lines {
			fn(l)
		}
	}
}

// The known values alone, as before the shapes: what Log costs more is
// the difference.
func BenchmarkString10MiB(b *testing.B) {
	benchLines(b, logLines(false), benchRedactor().String)
}

func BenchmarkLog10MiB(b *testing.B) {
	benchLines(b, logLines(false), benchRedactor().Log)
}

func BenchmarkStreamLine10MiB(b *testing.B) {
	benchLines(b, logLines(false), benchRedactor().Stream().Line)
}

func BenchmarkStreamLineShapes10MiB(b *testing.B) {
	benchLines(b, logLines(true), benchRedactor().Stream().Line)
}
