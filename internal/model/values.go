package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ParseText reads text from a flag, an input's env:, or name=value output by
// the type it becomes: integer and number as numbers, boolean as true or
// false, object and array as JSON.
func ParseText(s, typ string) (any, error) {
	switch typ {
	case "integer":
		i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", s)
		}
		return i, nil
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		return wholeNumber(f), nil
	case "boolean":
		switch s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not true or false", s)
	case "object", "array":
		var v any
		if err := decodeJSON([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("%q is not JSON", s)
		}
		v = numbers(v)
		if typ == "object" {
			if _, ok := v.(map[string]any); !ok {
				return nil, fmt.Errorf("%q is not a JSON object", s)
			}
		} else if _, ok := v.([]any); !ok {
			return nil, fmt.Errorf("%q is not a JSON array", s)
		}
		return v, nil
	}
	return s, nil
}

// TypeName names the type of a value the way schemas do.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int64:
		return "integer"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Conform checks v against the input's schema and returns it with defaults
// filled in. nil means not given.
func (in *Input) Conform(v any) (any, error) {
	if v == nil {
		switch {
		case in.Default != nil:
			v = in.Default
		case in.Required:
			return nil, fmt.Errorf("required")
		default:
			return nil, nil
		}
	}
	switch in.Type {
	case "":
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, typeError(in.Type, v)
		}
		if in.Pattern != "" {
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
				return nil, fmt.Errorf("pattern %q: %w", in.Pattern, err)
			}
			if !re.MatchString(s) {
				return nil, fmt.Errorf("%q does not match %s", s, in.Pattern)
			}
		}
	case "integer", "number":
		if f, ok := v.(float64); ok && in.Type == "integer" {
			v = wholeNumber(f)
		}
		var n float64
		switch x := v.(type) {
		case int64:
			n = float64(x)
		case float64:
			if in.Type == "integer" {
				return nil, typeError(in.Type, v)
			}
			n = x
		default:
			return nil, typeError(in.Type, v)
		}
		if in.Minimum != nil && n < *in.Minimum {
			return nil, fmt.Errorf("%v is less than %v", v, *in.Minimum)
		}
		if in.Maximum != nil && n > *in.Maximum {
			return nil, fmt.Errorf("%v is more than %v", v, *in.Maximum)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return nil, typeError(in.Type, v)
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return nil, typeError(in.Type, v)
		}
		if in.Items != nil {
			out := make([]any, len(list))
			for i, e := range list {
				c, err := in.Items.Conform(e)
				if err != nil {
					return nil, fmt.Errorf("[%d]: %w", i, err)
				}
				out[i] = c
			}
			v = out
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, typeError(in.Type, v)
		}
		if len(in.Properties) > 0 {
			out := make(map[string]any, len(obj))
			for k, e := range obj {
				out[k] = e
			}
			for name, ps := range in.Properties {
				c, err := ps.Conform(obj[name])
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				if c != nil {
					out[name] = c
				}
			}
			v = out
		}
	}
	if len(in.Enum) > 0 && !slices.ContainsFunc(in.Enum, func(e any) bool { return sameJSON(e, v) }) {
		return nil, fmt.Errorf("%s is not one of %s", jsonText(v), jsonText(in.Enum))
	}
	return v, nil
}

func typeError(want string, v any) error {
	return fmt.Errorf("expected %s, got %s", want, TypeName(v))
}

func sameJSON(a, b any) bool { return jsonText(a) == jsonText(b) }

// jsonText writes compact JSON with sorted keys.
func jsonText(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ReservedEnv reports whether a variable belongs to the program contract:
// env: cannot set OPSTRUSS_* and INPUT_*, and pass: never lets them
// through, so new facts never collide with a user's variables.
func ReservedEnv(name string) bool {
	return strings.HasPrefix(name, "OPSTRUSS_") || strings.HasPrefix(name, "INPUT_")
}

// InputEnv names the variable that holds an input for a program:
// upper case, with - as _, so a shell can read it.
func InputEnv(name string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
