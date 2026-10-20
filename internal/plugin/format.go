package plugin

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
)

// The formats of outputs, stdin, and the files of read and write: text,
// json, yaml, lines, and dotenv.

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Decode reads data in format: text as a string without its trailing
// newlines, json and yaml as their values, lines as a list of one string
// per line, and dotenv as an object of KEY=value lines, as .env files of
// Compose and EnvironmentFile of systemd. Bytes that are not UTF-8 become
// U+FFFD. Empty json is null.
func Decode(data []byte, format string) (any, error) {
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	switch format {
	case "", "text":
		return strings.TrimRight(text, "\n"), nil
	case "json":
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return expr.FromJSON([]byte(text))
	case "yaml":
		return expr.FromYAML([]byte(text))
	case "lines":
		items := []any{}
		if text = strings.TrimRight(text, "\n"); text == "" {
			return items, nil
		}
		for _, l := range strings.Split(text, "\n") {
			items = append(items, strings.TrimSuffix(l, "\r"))
		}
		return items, nil
	case "dotenv":
		return decodeDotenv(text)
	}
	return nil, fmt.Errorf("unknown format %s", format)
}

// Encode writes v in format for a program to read. Text and every line end
// with a newline, so that read in a shell sees a whole line.
func Encode(v any, format string) ([]byte, error) {
	switch format {
	case "", "text":
		s := expr.Text(v)
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		return []byte(s), nil
	case "json":
		return []byte(expr.JSON(v) + "\n"), nil
	case "yaml":
		return expr.ToYAML(v)
	case "lines":
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("lines writes an array, not %s", model.TypeName(v))
		}
		var b strings.Builder
		for i, it := range items {
			s := expr.Text(it)
			if strings.ContainsAny(s, "\r\n") {
				return nil, fmt.Errorf("item %d holds a newline, which lines cannot write", i)
			}
			b.WriteString(s + "\n")
		}
		return []byte(b.String()), nil
	case "dotenv":
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("dotenv writes an object, not %s", model.TypeName(v))
		}
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			if !envName.MatchString(k) {
				return nil, fmt.Errorf("dotenv cannot write the key %q, which is not a variable name", k)
			}
			b.WriteString(k + "=" + quoteDotenv(expr.Text(obj[k])) + "\n")
		}
		return []byte(b.String()), nil
	}
	return nil, fmt.Errorf("unknown format %s", format)
}

// decodeDotenv reads KEY=value lines; # starts a comment line, export
// before a key is allowed, single quotes keep their text as it is, and
// double quotes read \n, \r, \t, \", and \\.
func decodeDotenv(text string) (any, error) {
	out := map[string]any{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if k = strings.TrimSpace(k); !ok || !envName.MatchString(k) {
			return nil, fmt.Errorf("line %d is not KEY=value", i+1)
		}
		v = strings.TrimSpace(v)
		if n := len(v); n >= 2 && v[0] == v[n-1] && (v[0] == '"' || v[0] == '\'') {
			quote := v[0]
			v = v[1 : n-1]
			if quote == '"' {
				v = unescape.Replace(v)
			}
		}
		out[k] = v
	}
	return out, nil
}

var (
	unescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n", `\r`, "\r", `\t`, "\t")
	escape   = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
)

// quoteDotenv writes a value bare when no reader of .env files treats any
// of it specially, in single quotes when they keep it as it is, and in
// double quotes with escapes otherwise.
func quoteDotenv(s string) string {
	switch {
	case !strings.ContainsAny(s, " \t\r\n\"'\\#$`"):
		return s
	case !strings.ContainsAny(s, "'\r\n"):
		return "'" + s + "'"
	}
	return `"` + escape.Replace(s) + `"`
}
