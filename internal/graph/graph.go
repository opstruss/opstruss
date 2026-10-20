// Package graph orders nodes by their dependencies and names the instances
// of units.
package graph

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Graph is nodes in an order where each comes after its dependencies.
type Graph struct {
	Order []string
	Deps  map[string][]string
}

// Build orders nodes, ties by name, so the same project always runs in the
// same order. deps may name only nodes; a node depending on itself or a
// longer cycle is an error that names the cycle.
func Build(kind string, deps map[string][]string) (*Graph, error) {
	g := &Graph{Deps: map[string][]string{}}
	indeg := map[string]int{}
	users := map[string][]string{}
	for n, ds := range deps {
		set := slices.Sorted(slices.Values(ds))
		set = slices.Compact(set)
		g.Deps[n] = set
		indeg[n] = len(set)
		for _, d := range set {
			if _, ok := deps[d]; !ok {
				return nil, fmt.Errorf("%s %s depends on %s, which does not exist", kind, n, d)
			}
			users[d] = append(users[d], n)
		}
	}
	var ready []string
	for n, k := range indeg {
		if k == 0 {
			ready = append(ready, n)
		}
	}
	for len(ready) > 0 {
		slices.Sort(ready)
		n := ready[0]
		ready = ready[1:]
		g.Order = append(g.Order, n)
		for _, u := range users[n] {
			if indeg[u]--; indeg[u] == 0 {
				ready = append(ready, u)
			}
		}
	}
	if len(g.Order) < len(deps) {
		return nil, fmt.Errorf("%s %s form a cycle", kind, strings.Join(cycle(g.Deps, indeg), " → "))
	}
	return g, nil
}

// cycle finds one cycle among the nodes left with dependencies, as a path
// that ends where it starts.
func cycle(deps map[string][]string, indeg map[string]int) []string {
	left := slices.Sorted(maps.Keys(indeg))
	left = slices.DeleteFunc(left, func(n string) bool { return indeg[n] == 0 })
	state := map[string]int{}
	var stack []string
	var found []string
	var visit func(string) bool
	visit = func(n string) bool {
		state[n] = 1
		stack = append(stack, n)
		for _, d := range deps[n] {
			if indeg[d] == 0 {
				continue
			}
			switch state[d] {
			case 1:
				i := slices.Index(stack, d)
				found = append(slices.Clone(stack[i:]), d)
				return true
			case 0:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = 2
		return false
	}
	for _, n := range left {
		if state[n] == 0 && visit(n) {
			return found
		}
	}
	return left
}

// Key is the pattern of a for_each key of a unit, which becomes part of an
// address and of a path in the state.
var Key = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Keys returns the instance keys of a unit's for_each value: the items of a
// list of strings, or the keys of a map, in key order.
func Keys(v any) ([]string, error) {
	var keys []string
	switch x := v.(type) {
	case []any:
		for i, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("for_each item %d is not a string; write a map with keys of your own", i)
			}
			keys = append(keys, s)
		}
	case map[string]any:
		keys = slices.Collect(maps.Keys(x))
	default:
		return nil, fmt.Errorf("for_each is a list or a map")
	}
	slices.Sort(keys)
	for i, k := range keys {
		if !Key.MatchString(k) {
			return nil, fmt.Errorf("for_each key %q is not lowercase letters, digits, ., -, and _; use a map, or transformMapEntry, to pick keys", k)
		}
		if i > 0 && keys[i-1] == k {
			return nil, fmt.Errorf("for_each key %q appears twice", k)
		}
	}
	return keys, nil
}

// Address returns the address of a unit instance: <stack>/<unit>, or
// <stack>/<unit>[<key>] for an instance of for_each.
func Address(stack, unit, key string) string {
	if key == "" {
		return stack + "/" + unit
	}
	return stack + "/" + unit + "[" + key + "]"
}
