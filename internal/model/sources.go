package model

import (
	"path"
	"slices"
)

func matchesPass(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(p string) bool { ok, _ := path.Match(p, name); return ok })
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
