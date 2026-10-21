package secret

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/opstruss/opstruss/internal/expr"
)

// Mask is what a sensitive value becomes in what OpsTruss writes out.
const Mask = "***"

// MinLen is the length of the shortest value that redaction matches, since
// a shorter one would hide ordinary text.
const MinLen = 4

// Redactor hides the sensitive values that a run knows in what the run
// writes out: events, records, messages, and outputs. Each line of a value
// is matched as it is and in its JSON-escaped form, so a value that a
// program prints, also inside JSON, is hidden.
type Redactor struct {
	mu   sync.RWMutex
	pats []string // longest first, so a value that holds another is hidden whole
}

// NewRedactor returns a Redactor that knows no values yet.
func NewRedactor() *Redactor { return &Redactor{} }

// Add makes v, a secret or a sensitive value that the run was given,
// hidden: each string and number in it, line by line. It reports whether v
// is a string or number shorter than MinLen, which is not hidden.
func (r *Redactor) Add(v any) (short bool) {
	return r.add(v, false)
}

// AddDerived makes v, a value computed from sensitive ones, hidden. A
// string that holds a value known already is left as it is, so that only
// that value is hidden in it and the rest stays readable.
func (r *Redactor) AddDerived(v any) {
	r.add(v, true)
}

func (r *Redactor) add(v any, derived bool) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			r.add(e, derived)
		}
	case []any:
		for _, e := range x {
			r.add(e, derived)
		}
	case string, int64, float64:
		text := expr.Text(x)
		short := false
		for line := range strings.Lines(text) {
			line = strings.TrimSpace(line)
			if utf8.RuneCountInString(line) < MinLen {
				short = short || (line != "" && !strings.Contains(text, "\n"))
				continue
			}
			if derived && r.String(line) != line {
				continue
			}
			r.addPattern(line)
			for _, esc := range escapes(line) {
				r.addPattern(esc)
			}
		}
		return short
	}
	return false
}

// escapes are the forms s takes inside a JSON string, and quoted as Go
// quotes values in error messages, where they differ from s.
func escapes(s string) []string {
	var out []string
	if b, err := json.Marshal(s); err == nil && string(b[1:len(b)-1]) != s {
		out = append(out, string(b[1:len(b)-1]))
	}
	if q := strconv.Quote(s); q[1:len(q)-1] != s && !slices.Contains(out, q[1:len(q)-1]) {
		out = append(out, q[1:len(q)-1])
	}
	return out
}

func (r *Redactor) addPattern(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, found := slices.BinarySearchFunc(r.pats, p, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	if !found {
		r.pats = slices.Insert(r.pats, i, p)
	}
}

// String returns s with every known value in it replaced by Mask.
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.pats {
		if strings.Contains(s, p) {
			s = strings.ReplaceAll(s, p, Mask)
		}
	}
	return s
}

// Value returns a copy of v with every known value in its strings, keys
// included, replaced by Mask.
func (r *Redactor) Value(v any) any {
	switch x := v.(type) {
	case string:
		return r.String(x)
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[r.String(k)] = r.Value(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.Value(e)
		}
		return out
	case int64, float64:
		if text := expr.Text(x); r.String(text) != text {
			return Mask
		}
	}
	return v
}

// Contains reports whether a known value is in v: in a string, a key, or
// the text of a number.
func (r *Redactor) Contains(v any) bool {
	switch x := v.(type) {
	case string:
		return r.String(x) != x
	case map[string]any:
		for k, e := range x {
			if r.Contains(k) || r.Contains(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if r.Contains(e) {
				return true
			}
		}
	case int64, float64:
		return r.Contains(expr.Text(x))
	}
	return false
}

// Err returns err with its message redacted; errors.Is and errors.As
// still see what it wraps.
func (r *Redactor) Err(err error) error {
	if err == nil {
		return nil
	}
	msg := r.String(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redacted{err: err, msg: msg}
}

type redacted struct {
	err error
	msg string
}

func (e *redacted) Error() string { return e.msg }
func (e *redacted) Unwrap() error { return e.err }
