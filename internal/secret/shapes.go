package secret

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// shapes are the forms of well-known credentials, each with a prefix of
// its own and a length and alphabet of its own, which a run hides in what
// it writes out although it does not know them, as GitHub secret scanning
// and gitleaks find them. The list may grow; it is no contract.
var shapes = regexp.MustCompile(`\b(?:` +
	`gh[pousr]_[0-9A-Za-z]{36,251}|github_pat_[0-9A-Za-z_]{82,244}|` + // GitHub
	`glpat-[0-9A-Za-z_-]{20,249}|` + // GitLab
	`xox[abpr]-[0-9A-Za-z-]{20,250}|` + // Slack
	`[rs]k_live_[0-9A-Za-z]{24,247}|` + // Stripe
	`sk-ant-[0-9A-Za-z_-]{32,248}` + // Anthropic
	`)`)

// prefixes are what a text holds when it may hold a shape, which is
// cheaper to look for than the shapes.
var prefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "k_live_", "sk-ant-"}

// keyBegin and keyEnd are the lines around a private key in PEM, also an
// OpenSSH or PGP one.
var (
	keyBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----`)
	keyEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]{0,40}PRIVATE KEY(?: BLOCK)?-----`)
)

// maxShape is the longest text that a shape or a key line matches.
const maxShape = 256

// hiddenNames are the names of the variables whose values pass: lets
// through to a program a run hides as it hides a secret, as Buildkite's
// redacted vars do; a name matches whatever its case.
var hiddenNames = []string{"*_TOKEN", "*_SECRET", "*_PASSWORD", "*_ACCESS_KEY", "*_SECRET_KEY"}

// HiddenName reports whether the value of the variable name, which pass:
// lets through, is hidden as a secret is.
func HiddenName(name string) bool {
	name = strings.ToUpper(name)
	for _, p := range hiddenNames {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Log returns s as a run writes it out: the values it knows hidden, as
// String hides them, and the shapes of well-known credentials, and a
// private key from its BEGIN line to its END, or to the end of s. Only
// what a run writes out goes through Log; what the run hands on, and what
// decides that a value is sensitive, sees only the values it knows.
func (r *Redactor) Log(s string) string {
	s, _ = maskKeys(maskShapes(r.String(s)), false)
	return s
}

// LogValue is Value, with the strings in v as Log returns them.
func (r *Redactor) LogValue(v any) any {
	switch x := v.(type) {
	case string:
		return r.Log(x)
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[r.Log(k)] = r.LogValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.LogValue(e)
		}
		return out
	}
	return r.Value(v)
}

func maskShapes(s string) string {
	for _, p := range prefixes {
		if strings.Contains(s, p) {
			return shapes.ReplaceAllString(s, Mask)
		}
	}
	return s
}

// maskKeys hides the private keys in s, from each BEGIN line to its END,
// or to the end of s; in says that s starts inside a key, and the result
// whether it ends in one.
func maskKeys(s string, in bool) (string, bool) {
	if !in && !strings.Contains(s, "-----BEGIN ") {
		return s, false
	}
	var b strings.Builder
	for s != "" {
		if !in {
			loc := keyBegin.FindStringIndex(s)
			if loc == nil {
				b.WriteString(s)
				break
			}
			b.WriteString(s[:loc[0]])
			s, in = s[loc[0]:], true
			continue
		}
		b.WriteString(Mask)
		loc := keyEnd.FindStringIndex(s)
		if loc == nil {
			break
		}
		s, in = s[loc[1]:], false
	}
	return b.String(), in
}

// Stream hides, in the lines of one output stream, what Log hides, and
// each line of a private key that spans lines, until its END line or the
// end of the stream. Each stream of a program has its own.
type Stream struct {
	r  *Redactor
	in bool // inside a private key
}

// Stream returns a Stream for one output stream of a program of the run.
func (r *Redactor) Stream() *Stream { return &Stream{r: r} }

// Line returns line, the next of the stream, as the run writes it out.
func (s *Stream) Line(line string) string {
	line, s.in = maskKeys(maskShapes(s.r.String(line)), s.in)
	return line
}

// Cut splits text, the start of a line too long to keep whole, into what
// can be written out now, as Line returns it, and what is kept to go on
// with the rest of the line. It cuts no known value, shape, or key line in
// two, and keeps what may be the start of one, so out may be empty.
func (s *Stream) Cut(text string) (out, keep string) {
	s.r.mu.RLock()
	hold := maxShape
	if len(s.r.pats) > 0 {
		hold = max(hold, len(s.r.pats[0]))
	}
	var spans [][]int
	for _, p := range s.r.pats {
		for i := 0; ; {
			j := strings.Index(text[i:], p)
			if j < 0 {
				break
			}
			spans = append(spans, []int{i + j, i + j + len(p)})
			i += j + 1
		}
	}
	s.r.mu.RUnlock()
	for _, re := range []*regexp.Regexp{shapes, keyBegin, keyEnd} {
		spans = append(spans, re.FindAllStringIndex(text, -1)...)
	}
	cut := len(text) - hold + 1
	for moved := true; moved && cut > 0; {
		moved = false
		for _, sp := range spans {
			if sp[0] < cut && sp[1] > cut {
				cut, moved = sp[0], true
			}
		}
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut <= 0 {
		return "", text
	}
	return s.Line(text[:cut]), text[cut:]
}
