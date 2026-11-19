package model

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
)

// backend reads the plugin that the backend of the scope uses and checks
// the inputs it gives, which are read before the state opens.
func (c *checker) backend() {
	p := c.p
	if p.component {
		return
	}
	c.globalBackend()
	b := p.Backend
	if b == nil || b.Local {
		return
	}
	path := []string{"backend"}
	pkg, err := c.backendPackage(b.Uses)
	switch {
	case errors.Is(err, errBroken):
		return
	case err != nil:
		c.add(at(path, "uses"), "%v", err)
		return
	case pkg.broken:
		return
	case pkg.Plugin == nil || pkg.Plugin.Backend == nil:
		c.add(at(path, "uses"), "%s is not a backend: a backend uses a plugin whose %s has backend:", b.Uses, PluginFile)
		return
	}
	b.Package = pkg
	c.with(at(path, "with"), at(path, "uses"), &Target{Name: b.Uses, Kind: TargetPlugin, Package: pkg}, b.With)
	c.environment(path, nil, b.Pass)
	pl := place{kind: "backend"}
	if p.Global {
		pl.global = "the backend of the project, which the global scope uses,"
	}
	for _, k := range slices.Sorted(maps.Keys(b.With)) {
		c.refs(at(path, "with", k), b.With[k], pl)
		if use, err := expr.AnalyzeValue(b.With[k]); err == nil && slices.Contains(use.Funcs, "kv") {
			c.add(at(path, "with", k), "with of a backend cannot call kv(), which reads the state that the backend opens")
		}
	}
}

// backendPackage finds what the backend uses: a source, a path, a name in
// uses:, or a package of a catalog that uses: names.
func (c *checker) backendPackage(uses string) (*Package, error) {
	switch {
	case strings.HasPrefix(uses, "github.com/"):
		return c.sourcePackage(uses, nil, uses)
	case localPath(uses):
		return c.pkg(uses, nil, filepath.Join(c.p.DirOf("backend"), uses))
	}
	if pkg, err := c.member(uses); pkg != nil || err != nil {
		return pkg, err
	}
	if pkg := c.p.Packages[uses]; pkg != nil {
		return pkg, nil
	}
	return nil, fmt.Errorf("uses: declares no %s%s", uses, expr.DidYouMean(uses, slices.Collect(maps.Keys(c.p.Packages))))
}

// globalBackend keeps the records of global units where every checkout
// finds them: a stack that shares its state through a plugin backend needs
// the global scope, which uses the backend at the top of the project, to
// share its state too, or each checkout would make the global units again.
func (c *checker) globalBackend() {
	p := c.p
	if _, plugin := p.root.value["backend"].(map[string]any); plugin {
		return
	}
	global := false
	for _, v := range p.top["units"] {
		global = global || isGlobal(v)
	}
	if !global {
		return
	}
	for _, s := range slices.Sorted(maps.Keys(p.Stacks)) {
		if b := p.Stacks[s].Backend; b != nil && !b.Local {
			c.add([]string{"stacks", s, "backend"}, "stack %s keeps its state in a plugin backend while the global units of the project keep theirs in the local backend of each checkout, which would make them again; write the backend at the top of %s, which the global scope uses, and backend: local in the stacks that keep their state here", s, FileName)
		}
	}
}

// backendLocal says why with of a backend cannot read local name, or "":
// it calls kv(), or reads what only a run or a plan knows, itself or
// through other locals.
func (c *checker) backendLocal(name string, seen map[string]bool) string {
	if seen[name] {
		return ""
	}
	seen[name] = true
	use, err := expr.AnalyzeValue(c.p.Locals[name])
	if err != nil {
		return ""
	}
	if slices.Contains(use.Funcs, "kv") {
		return "calls kv()"
	}
	for _, r := range use.Refs {
		other := ""
		if len(r.Path) > 1 {
			other = r.Path[1]
		}
		switch r.Path[0] {
		case "inputs":
		case "secrets":
			if s := c.p.Secrets[other]; s != nil && fromProgram(s) {
				return "reads secret " + other + ", which a program gives in a run"
			}
		case "locals":
			if other == "" {
				return "reads every local"
			}
			if why := c.backendLocal(other, seen); why != "" {
				return "reads local " + other + ", which " + why
			}
		default:
			return "reads " + r.Path[0]
		}
	}
	return ""
}
