package model

import (
	"path"
	"reflect"
	"slices"
)

// PluginBinaries maps plugin directories to their private executable paths.
func (p *Project) PluginBinaries() map[string][]string {
	out := map[string][]string{}
	var walk func(*Project)
	walk = func(p *Project) {
		for _, pkg := range p.Packages {
			if len(pkg.BinDirs) > 0 {
				out[pkg.Dir] = pkg.BinDirs
			}
		}
		for _, u := range p.Units {
			if t := u.Target; t != nil {
				if t.Package != nil && len(t.Package.BinDirs) > 0 {
					out[t.Package.Dir] = t.Package.BinDirs
				}
				if t.Component != nil {
					walk(t.Component)
				}
			}
		}
	}
	walk(p)
	return out
}

func matchesPass(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { ok, _ := path.Match(p, name); return ok })
}

// Asks reports whether the plugin asks for the variable name: env: holds
// names and patterns, as pass: does.
func (pl *Plugin) Asks(name string) bool { return matchesPass(pl.Env, name) }

// Reaches reports whether the pass: item p can let through a variable that
// the plugin asks for.
func (pl *Plugin) Reaches(p string) bool {
	return slices.ContainsFunc(pl.Env, func(asked string) bool {
		a, _ := path.Match(p, asked)
		b, _ := path.Match(asked, p)
		return a || b
	})
}

// Grants reports whether each remote boundary is authorized by its caller.
// Local declarations remain grants; a remote declaration is only a request.
func (n *Node) Grants(name string) bool {
	if n.In.Source == nil {
		return true
	}
	for caller := n.Parent; caller != nil; caller = caller.Parent {
		if caller.In.Source == nil {
			return matchesPass(caller.Def().Pass, name)
		}
		if !matchesPass(caller.Unit.Pass, name) {
			return false
		}
	}
	return false
}

// TaskGrants applies the caller's grants at a remote component task boundary.
func (n *Node) TaskGrants(name string) bool {
	if n.In.Source == nil {
		return matchesPass(n.Def().Pass, name)
	}
	return matchesPass(n.Unit.Pass, name) && n.Grants(name)
}

// SameDefinition compares saved models without mutable cache locks and leases.
func (p *Project) SameDefinition(other *Project) bool {
	var definition func(*Project) any
	definition = func(p *Project) any {
		if p == nil {
			return nil
		}
		packages := map[string]any{}
		for name, pkg := range p.Packages {
			packages[name] = []any{pkg.Dir, pkg.Plugin, pkg.Source, pkg.Catalog, pkg.Components, pkg.Builtin}
		}
		components := map[string]any{}
		for name, u := range p.Units {
			if u.Target != nil && u.Target.Component != nil {
				components[name] = definition(u.Target.Component)
			}
			if u.Target != nil && u.Target.Package != nil {
				pkg := u.Target.Package
				packages["units."+name] = []any{pkg.Dir, pkg.Plugin, pkg.Source, pkg.Catalog, pkg.Components, pkg.Builtin}
			}
		}
		origins := map[string]string{}
		for key := range p.origins {
			origins[key] = p.DirOf(key)
		}
		return []any{p.Dir, p.StackName, p.doc, p.Source, p.Catalog, packages, components, origins}
	}
	return reflect.DeepEqual(definition(p), definition(other))
}
