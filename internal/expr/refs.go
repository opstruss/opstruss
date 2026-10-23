package expr

import (
	"slices"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
)

// Ref is a path that an expression reads, such as
// [units web outputs url]. Dynamic means the path is indexed further with
// a key that is only known when the expression runs.
type Ref struct {
	Path    []string
	Dynamic bool
	Expr    string
}

// Use is what an expression reads and the functions it calls, with the
// names that its kv() calls read, "" for one known only when it runs.
type Use struct {
	Refs  []Ref
	Funcs []string
	KV    []string
}

// Analyze parses src and returns what it reads from the namespaces, and
// the functions it calls; it also checks that src compiles.
func Analyze(src string) (*Use, error) {
	e, err := envFor("")
	if err != nil {
		return nil, err
	}
	checked, iss := e.Compile(src)
	if iss.Err() != nil {
		return nil, compileError(src, iss.Err())
	}
	u := &Use{}
	u.walk(checked.NativeRep().Expr(), src)
	slices.Sort(u.Funcs)
	u.Funcs = slices.Compact(u.Funcs)
	return u, nil
}

// AnalyzeValue analyzes every expression in a string, map, or list.
func AnalyzeValue(v any) (*Use, error) {
	all := &Use{}
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case string:
			parts, err := Split(x)
			if err != nil {
				return err
			}
			for _, p := range parts {
				if !p.IsExpr {
					continue
				}
				u, err := Analyze(p.Expr)
				if err != nil {
					return err
				}
				all.Refs = append(all.Refs, u.Refs...)
				all.Funcs = append(all.Funcs, u.Funcs...)
				all.KV = append(all.KV, u.KV...)
			}
		case map[string]any:
			for _, k := range sortedKeys(x) {
				if err := walk(x[k]); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(v); err != nil {
		return nil, err
	}
	slices.Sort(all.Funcs)
	all.Funcs = slices.Compact(all.Funcs)
	return all, nil
}

func (u *Use) walk(e ast.Expr, src string) {
	if p, ok := selectPath(e); ok {
		if slices.Contains(Namespaces, p[0]) {
			u.Refs = append(u.Refs, Ref{Path: p, Expr: src})
		}
		return
	}
	switch e.Kind() {
	case ast.CallKind:
		c := e.AsCall()
		u.Funcs = append(u.Funcs, c.FunctionName())
		if c.FunctionName() == "kv" && len(c.Args()) >= 2 {
			name := ""
			if a := c.Args()[1]; a.Kind() == ast.LiteralKind {
				if s, ok := a.AsLiteral().(types.String); ok {
					name = string(s)
				}
			}
			u.KV = append(u.KV, name)
		}
		if c.FunctionName() == "_[_]" && len(c.Args()) == 2 {
			// A path indexed with a key known only at run time.
			if p, ok := selectPath(c.Args()[0]); ok && slices.Contains(Namespaces, p[0]) {
				u.Refs = append(u.Refs, Ref{Path: p, Dynamic: true, Expr: src})
				u.walk(c.Args()[1], src)
				return
			}
		}
		if c.IsMemberFunction() {
			u.walk(c.Target(), src)
		}
		for _, a := range c.Args() {
			u.walk(a, src)
		}
	case ast.SelectKind:
		u.walk(e.AsSelect().Operand(), src)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			u.walk(el, src)
		}
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			u.walk(me.Key(), src)
			u.walk(me.Value(), src)
		}
	case ast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			u.walk(f.AsStructField().Value(), src)
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		for _, x := range []ast.Expr{c.IterRange(), c.AccuInit(), c.LoopCondition(), c.LoopStep(), c.Result()} {
			u.walk(x, src)
		}
	}
}

// selectPath flattens field selections and constant indexes, such as
// units['my-db'].outputs.host, into names.
func selectPath(e ast.Expr) ([]string, bool) {
	switch e.Kind() {
	case ast.IdentKind:
		return []string{e.AsIdent()}, true
	case ast.SelectKind:
		s := e.AsSelect()
		if s.IsTestOnly() {
			return nil, false
		}
		p, ok := selectPath(s.Operand())
		if !ok {
			return nil, false
		}
		return append(p, s.FieldName()), true
	case ast.CallKind:
		c := e.AsCall()
		if c.FunctionName() != "_[_]" || len(c.Args()) != 2 || c.Args()[1].Kind() != ast.LiteralKind {
			return nil, false
		}
		key, ok := c.Args()[1].AsLiteral().(types.String)
		if !ok {
			return nil, false
		}
		p, ok := selectPath(c.Args()[0])
		if !ok {
			return nil, false
		}
		return append(p, string(key)), true
	}
	return nil, false
}
