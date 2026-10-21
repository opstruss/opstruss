package expr

import (
	"fmt"
	"sync"
)

// later is a value made the first time an expression reads it, and only
// then: a secret, which a run resolves only for what reads it, or a value
// computed from one. It is made once; an error fails each expression that
// reads it.
type later struct {
	once sync.Once
	make func() (any, error)
	v    any
	err  error
}

// Later returns a value that make makes when an expression first reads
// it. It may sit anywhere in the variables of a scope.
func Later(make func() (any, error)) any { return &later{make: make} }

// IsLater says whether v is a value that Later made.
func IsLater(v any) bool {
	_, ok := v.(*later)
	return ok
}

func (l *later) get() (any, error) {
	l.once.Do(func() { l.v, l.err = l.make() })
	return l.v, l.err
}

// ReadsLater reports whether an expression in v reads, in s, a value that
// Later makes, so what holds its result can wait as well.
func ReadsLater(v any, s *Scope) bool {
	switch x := v.(type) {
	case string:
		parts, err := Split(x)
		if err != nil {
			return false
		}
		for _, p := range parts {
			if !p.IsExpr {
				continue
			}
			use, err := analyzed(p.Expr)
			if err != nil {
				continue
			}
			for _, r := range use.Refs {
				if laterAt(s.Vars[r.Path[0]], r.Path[1:]) {
					return true
				}
			}
		}
	case map[string]any:
		for _, e := range x {
			if ReadsLater(e, s) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if ReadsLater(e, s) {
				return true
			}
		}
	}
	return false
}

// laterAt reports whether v holds a value that Later makes at path, or
// under it.
func laterAt(v any, path []string) bool {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		v = m[p]
	}
	return holdsLater(v)
}

func holdsLater(v any) bool {
	switch x := v.(type) {
	case *later:
		return true
	case map[string]any:
		for _, e := range x {
			if holdsLater(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if holdsLater(e) {
				return true
			}
		}
	}
	return false
}

// Made returns v with every value in it that Later makes made, for what
// reads v outside an expression.
func Made(v any) (any, error) {
	switch x := v.(type) {
	case *later:
		w, err := x.get()
		if err != nil {
			return nil, err
		}
		return Made(w)
	case map[string]any:
		if !holdsLater(x) {
			return x, nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			m, err := Made(e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = m
		}
		return out, nil
	case []any:
		if !holdsLater(x) {
			return x, nil
		}
		out := make([]any, len(x))
		for i, e := range x {
			m, err := Made(e)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = m
		}
		return out, nil
	}
	return v, nil
}
