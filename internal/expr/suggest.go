package expr

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Facts are the names under opstruss. that expressions may read.
var Facts = []string{"address", "dir", "cwd", "project", "artifacts", "temp", "state_key", "reason", "os", "arch", "host", "time", "run"}

// Nearest is the word of words that s is a typo of, at most two edits
// away, or "".
func Nearest(s string, words []string) string {
	best, bestD := "", 3
	for _, w := range words {
		if d := distance(s, w); d < bestD && d < len(w) {
			best, bestD = w, d
		}
	}
	return best
}

// DidYouMean is "; did you mean <word>?" for the word of words that s is a
// typo of, or "".
func DidYouMean(s string, words []string) string {
	if near := Nearest(s, words); near != "" {
		return "; did you mean " + near + "?"
	}
	return ""
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

var (
	undeclared = regexp.MustCompile(`undeclared reference to '([^']+)'`)
	// A member call, such as x.toJSON(), of a function that takes its
	// value as an argument.
	memberCall = regexp.MustCompile(`found no matching overload for '([^']+)' applied to '[^']*\.\(`)
	identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$`)
)

// argumentFunctions are the functions of OpsTruss, which take their value
// as an argument, not as a method.
var argumentFunctions = []string{"toJSON", "fromJSON", "toYAML", "fromYAML", "sha256", "file", "kv"}

// otherNames are the functions that other languages name as these do.
var otherNames = map[string]string{"length": "size", "len": "size", "count": "size", "lower": "lowerAscii", "upper": "upperAscii"}

// unknown is why src does not compile when the checker names something
// that no scope or function has: a function, or a name, with the one meant
// when it is near. ok is false for other errors.
func unknown(src, msg string) (string, bool) {
	if m := memberCall.FindStringSubmatch(msg); m != nil && slices.Contains(argumentFunctions, m[1]) {
		value := "<value>"
		if r := regexp.MustCompile(`([\w.]+)\.` + m[1] + `\(\s*\)`).FindStringSubmatch(src); r != nil {
			value = r[1]
		}
		return m[1] + " takes its value as an argument: " + m[1] + "(" + value + ")", true
	}
	m := undeclared.FindStringSubmatch(msg)
	if m == nil {
		return "", false
	}
	name := m[1]
	funcs := functionNames()
	// A function of a library, such as math.greatest, that is not there
	// reads as its library, a name that no scope has.
	if q := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\.(\w+)\s*\(`).FindStringSubmatch(src); q != nil &&
		slices.ContainsFunc(funcs, func(f string) bool { return strings.HasPrefix(f, name+".") }) {
		name += "." + q[1]
	}
	if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(src) {
		words := slices.Concat(funcs, slices.Collect(maps.Keys(otherNames)))
		if lib, _, ok := strings.Cut(name, "."); ok {
			// A macro of a library, such as math.greatest, is named
			// without it.
			for _, f := range funcs {
				words = append(words, lib+"."+f)
			}
		}
		near := Nearest(name, words)
		if to, ok := otherNames[near]; ok {
			near = to
		}
		if near != "" {
			return "there is no function " + name + "; did you mean " + near + "?", true
		}
		return "there is no function " + name, true
	}
	msg = "there is no name " + name + "; an expression reads " + strings.Join(Namespaces[:len(Namespaces)-1], ", ") +
		", and " + Namespaces[len(Namespaces)-1]
	switch {
	case slices.Contains(Facts, name):
		msg += "; did you mean opstruss." + name + "?"
	case Nearest(name, Namespaces) != "":
		msg += DidYouMean(name, Namespaces)
	case Nearest(name, Facts) != "":
		msg += "; did you mean opstruss." + Nearest(name, Facts) + "?"
	case name == "env":
		msg += "; a value of the environment comes in as an input or a secret with env:"
	}
	return msg, true
}

// functionNames are the names of the functions and macros of expressions.
func functionNames() []string {
	e, err := env()
	if err != nil {
		return nil
	}
	names := []string{"file"}
	for name := range e.Functions() {
		if identifier.MatchString(name) {
			names = append(names, name)
		}
	}
	for _, m := range e.Macros() {
		names = append(names, m.Function())
	}
	slices.Sort(names)
	return slices.Compact(names)
}
