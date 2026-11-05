package expr

import (
	"slices"

	"cel.dev/cel-go/cel"
)

// Surface lists what expressions offer, one fact a line: each overload of
// each function, by its name and the id of the overload, each macro, each
// namespace, and each fact under opstruss. The surface test keeps every
// line of a release, so that a later version cannot take one away.
func Surface() ([]string, error) {
	base, err := env()
	if err != nil {
		return nil, err
	}
	// file() is bound to the directory of each expression; any one shows it.
	e, err := base.Extend(unary("file", cel.StringType, cel.StringType, readFile(".")))
	if err != nil {
		return nil, err
	}
	var lines []string
	for name, fn := range e.Functions() {
		// kv() is a macro, and its function takes a reader that only the
		// macro passes.
		if name == "kv" {
			continue
		}
		for _, o := range fn.OverloadDecls() {
			lines = append(lines, "function "+name+" "+o.ID())
		}
	}
	for _, m := range e.Macros() {
		lines = append(lines, "macro "+m.MacroKey())
	}
	for _, n := range Namespaces {
		lines = append(lines, "variable "+n)
	}
	for _, f := range Facts {
		lines = append(lines, "variable opstruss."+f)
	}
	slices.Sort(lines)
	return slices.Compact(lines), nil
}
