// Package expr evaluates ${{ }}: CEL with the namespaces and functions of
// OpsTruss, the typing rules of whole values and text, and the references
// of an expression, which are dependencies.
package expr

import (
	"fmt"
	"strings"
)

// Part is a piece of a string: text, or the source of one expression.
type Part struct {
	Text   string
	Expr   string
	IsExpr bool
}

// Split splits s into text and ${{ }} expressions. $${{ writes ${{ as text.
// An expression ends at the first }} outside quotes and braces, so map
// literals such as ${{ {'a': {'b': 1}} }} stay whole.
func Split(s string) ([]Part, error) {
	var parts []Part
	var text strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${{"):
			text.WriteString("${{")
			i += 4
		case strings.HasPrefix(s[i:], "${{"):
			end, err := exprEnd(s, i+3)
			if err != nil {
				return nil, err
			}
			src := strings.TrimSpace(s[i+3 : end])
			if src == "" {
				return nil, fmt.Errorf("empty ${{ }} in %q", s)
			}
			if text.Len() > 0 {
				parts = append(parts, Part{Text: text.String()})
				text.Reset()
			}
			parts = append(parts, Part{Expr: src, IsExpr: true})
			i = end + 2
		default:
			text.WriteByte(s[i])
			i++
		}
	}
	if text.Len() > 0 {
		parts = append(parts, Part{Text: text.String()})
	}
	return parts, nil
}

func exprEnd(s string, i int) (int, error) {
	depth := 0
	var quote byte
	for ; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
				continue
			}
			if i+1 < len(s) && s[i+1] == '}' {
				return i, nil
			}
			return 0, fmt.Errorf("a single } closes nothing in %q", s)
		}
	}
	return 0, fmt.Errorf("${{ is not closed in %q", s)
}

// Whole returns the expression of a string that is exactly one ${{ }},
// whose result keeps its type.
func Whole(s string) (string, bool) {
	parts, err := Split(s)
	if err != nil || len(parts) != 1 || !parts[0].IsExpr {
		return "", false
	}
	return parts[0].Expr, true
}

// HasExpr reports whether s holds a ${{ }}.
func HasExpr(s string) bool {
	parts, err := Split(s)
	if err != nil {
		return true
	}
	for _, p := range parts {
		if p.IsExpr {
			return true
		}
	}
	return false
}
