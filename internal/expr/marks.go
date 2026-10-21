package expr

import (
	"fmt"
	"sync"
)

// Marks say which parts of a value are sensitive: true for all of it, a
// map[string]any with the marks of the keys that have sensitive parts, for
// a map, or nil for none. The result of an expression that reads a
// sensitive part is sensitive as a whole, like a sensitive value in
// Terraform.

// MarkAt returns the marks of the part of a value at path.
func MarkAt(marks any, path ...string) any {
	for _, p := range path {
		m, ok := marks.(map[string]any)
		if !ok {
			// true covers every part; nil none.
			return marks
		}
		marks = m[p]
	}
	if m, ok := marks.(map[string]any); ok && len(m) == 0 {
		return nil
	}
	return marks
}

// Sensitive reports whether the expression src reads a sensitive part of
// s.
func Sensitive(src string, s *Scope) bool {
	if len(s.Marks) == 0 {
		return false
	}
	use, err := analyzed(src)
	if err != nil {
		// An expression that does not compile fails anyway; until then it
		// may read anything.
		return true
	}
	for _, r := range use.Refs {
		if MarkAt(s.Marks, r.Path...) != nil {
			return true
		}
	}
	return false
}

var analyses sync.Map // src → *Use

func analyzed(src string) (*Use, error) {
	if u, ok := analyses.Load(src); ok {
		return u.(*Use), nil
	}
	u, err := Analyze(src)
	if err != nil {
		return nil, err
	}
	analyses.Store(src, u)
	return u, nil
}

// EvalMarked evaluates every expression in a string, map, or list, as
// EvalValue does, and returns the marks of the result: a string with an
// expression that reads a sensitive part is sensitive, a map keeps the
// marks of its keys, and a list with a sensitive item is sensitive.
func EvalMarked(v any, s *Scope) (any, any, error) {
	switch x := v.(type) {
	case string:
		out, err := EvalString(x, s)
		if err != nil {
			return nil, nil, err
		}
		return out, stringMarks(x, s), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		marks := map[string]any{}
		for _, k := range sortedKeys(x) {
			e, m, err := EvalMarked(x[k], s)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = e
			if m != nil {
				marks[k] = m
			}
		}
		if len(marks) == 0 {
			return out, nil, nil
		}
		return out, marks, nil
	case []any:
		out := make([]any, len(x))
		var marks any
		for i, e := range x {
			r, m, err := EvalMarked(e, s)
			if err != nil {
				return nil, nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
			if m != nil {
				marks = true
			}
		}
		return out, marks, nil
	}
	return v, nil, nil
}

// EvalTextMarked evaluates a value that is always a string, as EvalText
// does, and reports whether it is sensitive.
func EvalTextMarked(str string, s *Scope) (string, bool, error) {
	text, err := EvalText(str, s)
	if err != nil {
		return "", false, err
	}
	return text, stringMarks(str, s) != nil, nil
}

func stringMarks(str string, s *Scope) any {
	if len(s.Marks) == 0 {
		return nil
	}
	parts, err := Split(str)
	if err != nil {
		return true
	}
	for _, p := range parts {
		if p.IsExpr && Sensitive(p.Expr, s) {
			return true
		}
	}
	return nil
}
