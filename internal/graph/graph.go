// Package graph orders nodes by their dependencies and names the instances
// of units.
package graph

import (
	"errors"
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
		var errs []error
		for _, c := range cycles(g.Deps, indeg) {
			errs = append(errs, fmt.Errorf("%s %s form a cycle", kind, strings.Join(c, " → ")))
		}
		return nil, errors.Join(errs...)
	}
	return g, nil
}

// cycles finds each cycle among the nodes left with dependencies: for each
// group of nodes that reach one another, a shortest path from its first
// node by name back to it, in the order of those nodes.
func cycles(deps map[string][]string, indeg map[string]int) [][]string {
	left := map[string]bool{}
	for n, k := range indeg {
		if k > 0 {
			left[n] = true
		}
	}
	// The groups are the strongly connected components, found as Tarjan
	// finds them.
	index, low, on := map[string]int{}, map[string]int{}, map[string]bool{}
	var stack []string
	var groups [][]string
	var visit func(string)
	visit = func(n string) {
		index[n], low[n] = len(index), len(index)
		stack = append(stack, n)
		on[n] = true
		for _, d := range deps[n] {
			if !left[d] {
				continue
			}
			if _, seen := index[d]; !seen {
				visit(d)
				low[n] = min(low[n], low[d])
			} else if on[d] {
				low[n] = min(low[n], index[d])
			}
		}
		if low[n] != index[n] {
			return
		}
		var group []string
		for m := ""; m != n; {
			m = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			on[m] = false
			group = append(group, m)
		}
		groups = append(groups, group)
	}
	for _, n := range slices.Sorted(maps.Keys(left)) {
		if _, seen := index[n]; !seen {
			visit(n)
		}
	}
	var out [][]string
	for _, group := range groups {
		start := slices.Min(group)
		if len(group) == 1 && !slices.Contains(deps[start], start) {
			// A node after a cycle, not in one.
			continue
		}
		out = append(out, loop(deps, group, start))
	}
	slices.SortFunc(out, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return out
}

// loop is a shortest path from start back to start through the nodes of
// group.
func loop(deps map[string][]string, group []string, start string) []string {
	parent := map[string]string{}
	queue := []string{start}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, d := range deps[n] {
			switch _, seen := parent[d]; {
			case d == start:
				var back []string
				for m := n; m != start; m = parent[m] {
					back = append(back, m)
				}
				slices.Reverse(back)
				return append(append([]string{start}, back...), start)
			case !seen && slices.Contains(group, d):
				parent[d] = n
				queue = append(queue, d)
			}
		}
	}
	return group
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
