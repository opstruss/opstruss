package model

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/graph"
)

// Node is a unit at its path in the project: a unit of the project, such
// as web, or a unit of a component that a unit uses, such as site/db.
type Node struct {
	Path   string
	Name   string
	Unit   *Unit
	In     *Project // the project or component whose files write the unit
	Parent *Node    // the unit that uses the component this one is in; nil in the project

	// Reads are the units of the same file that its expressions read or
	// that it needs, which units.<name> holds for it.
	Reads []string
}

// Component is the component that n uses, or nil.
func (n *Node) Component() *Project {
	if t := n.Unit.Target; t != nil && t.Kind == TargetComponent {
		return t.Component
	}
	return nil
}

// Dir is the directory of the file that writes the unit.
func (n *Node) Dir() string { return n.In.DirOf("units." + n.Name) }

// Def is the unit as it runs, with what the units that use the components
// it is in give it: protect when any of them writes it; approval ask when
// any writes ask, otherwise auto when any writes auto, so a component only
// narrows what its user decides; timeout, retry, and stop from the nearest
// that writes them; pass from all. Env stays the unit's own, since each is
// read where it is written.
func (n *Node) Def() *Unit {
	if n.Parent == nil {
		return n.Unit
	}
	u := *n.Unit
	for a := n.Parent; a != nil; a = a.Parent {
		au := a.Unit
		u.Protect = u.Protect || au.Protect
		switch {
		case au.Approval == "ask":
			u.Approval = "ask"
		case u.Approval == "":
			u.Approval = au.Approval
		}
		u.Timeout = cmp.Or(u.Timeout, au.Timeout)
		if u.Retry == nil {
			u.Retry = au.Retry
		}
		if u.Stop == nil {
			u.Stop = au.Stop
		}
		if n.In.Source == nil {
			u.Pass = append(slices.Clone(u.Pass), au.Pass...)
		}
	}
	return &u
}

// Segment is one step of an address: a unit and the key of its instance.
type Segment struct {
	Unit string
	Key  string
}

// ParseAddress splits an address without its stack, such as site[eu]/db,
// into its segments.
func ParseAddress(id string) []Segment {
	var out []Segment
	for _, s := range strings.Split(id, "/") {
		unit, key, _ := strings.Cut(strings.TrimSuffix(s, "]"), "[")
		out = append(out, Segment{Unit: unit, Key: key})
	}
	return out
}

// NodeAt returns the node at an address without its stack, or nil when the
// files have no unit there.
func (p *Project) NodeAt(id string) *Node {
	segs := ParseAddress(id)
	names := make([]string, len(segs))
	for i, s := range segs {
		names[i] = s.Unit
	}
	return p.Nodes[strings.Join(names, "/")]
}

// unitAt says whether the files have a unit at an address without its
// stack, whatever its keys, also inside the components that units use.
func (p *Project) unitAt(id string) bool {
	in := p
	segs := ParseAddress(id)
	for i, s := range segs {
		u := in.Units[s.Unit]
		switch {
		case u == nil:
			return false
		case i == len(segs)-1:
			return true
		case u.Target == nil || u.Target.Component == nil:
			return false
		}
		in = u.Target.Component
	}
	return false
}

// Leaves are the paths of the units that run at path or inside it, in
// graph order.
func (p *Project) Leaves(path string) []string {
	var out []string
	for _, l := range p.Order {
		if l == path || strings.HasPrefix(l, path+"/") {
			out = append(out, l)
		}
	}
	return out
}

// graph builds the graph of the units that run, through the components
// that units use, as Terraform does through modules: a unit depends on the
// units its expressions read and those it needs; reading an output of a
// component depends on what that output reads, and an input of a component
// on what the unit that uses it gives there. What a unit that uses a
// component needs, and what its env, if, and for_each read, reach every
// unit inside.
func (c *checker) graph() {
	p := c.p
	p.Nodes = map[string]*Node{}
	var add func(in *Project, parent *Node)
	add = func(in *Project, parent *Node) {
		for _, name := range slices.Sorted(maps.Keys(in.Units)) {
			n := &Node{Path: name, Name: name, Unit: in.Units[name], In: in, Parent: parent}
			if parent != nil {
				n.Path = parent.Path + "/" + name
			}
			p.Nodes[n.Path] = n
			if comp := n.Component(); comp != nil {
				add(comp, n)
			}
		}
	}
	add(p, nil)
	deps := map[string][]string{}
	for _, path := range slices.Sorted(maps.Keys(p.Nodes)) {
		n := p.Nodes[path]
		reads := slices.Clone(n.Unit.Needs)
		for _, v := range unitValues(n.Unit) {
			if use, err := expr.AnalyzeValue(v); err == nil {
				for _, r := range use.Refs {
					if r.Path[0] == "units" && len(r.Path) > 1 {
						reads = append(reads, r.Path[1])
					}
				}
			}
		}
		slices.Sort(reads)
		n.Reads = slices.Compact(reads)
		if n.Component() == nil {
			deps[path] = c.nodeDeps(n)
		}
	}
	g, err := graph.Build("units", deps)
	if err != nil {
		c.addEach([]string{"units"}, err)
		return
	}
	p.Order, p.Deps = g.Order, g.Deps
}

// nodeDeps are the units that run which n depends on.
func (c *checker) nodeDeps(n *Node) []string {
	seen := map[localKey]bool{}
	var out []string
	for _, v := range unitValues(n.Unit) {
		out = append(out, c.dataDeps(v, n.In, n.Parent, seen)...)
	}
	for _, need := range n.Unit.Needs {
		out = append(out, c.unitDeps(n.Parent, need, nil, seen)...)
	}
	for a := n.Parent; a != nil; a = a.Parent {
		for _, v := range []any{envValues(a.Unit.Env), condition(a.Unit.If), a.Unit.ForEach} {
			out = append(out, c.dataDeps(v, a.In, a.Parent, seen)...)
		}
		for _, need := range a.Unit.Needs {
			out = append(out, c.unitDeps(a.Parent, need, nil, seen)...)
		}
	}
	return out
}

// localKey is a local of the component that a node uses, or of the
// project for nil.
type localKey struct {
	at   *Node
	name string
}

// dataDeps are the units that run whose outputs v reads, written in the
// file of in, inside the component that at uses, or in the project for
// nil: a unit read, the units that a component output read comes from, and
// those that an input or a local read comes from.
func (c *checker) dataDeps(v any, in *Project, at *Node, seen map[localKey]bool) []string {
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range use.Refs {
		switch r.Path[0] {
		case "units":
			if len(r.Path) > 1 {
				out = append(out, c.unitDeps(at, r.Path[1], r.Path[2:], seen)...)
			}
		case "inputs":
			if at == nil {
				continue
			}
			for _, k := range slices.Sorted(maps.Keys(at.Unit.With)) {
				if len(r.Path) == 1 || r.Path[1] == k {
					out = append(out, c.dataDeps(at.Unit.With[k], at.In, at.Parent, seen)...)
				}
			}
		case "locals":
			for _, k := range slices.Sorted(maps.Keys(in.Locals)) {
				lk := localKey{at, k}
				if (len(r.Path) == 1 || r.Path[1] == k) && !seen[lk] {
					seen[lk] = true
					out = append(out, c.dataDeps(in.Locals[k], in, at, seen)...)
				}
			}
		}
	}
	return out
}

// unitDeps are the units that run behind units.<name><rest> read inside
// the component that at uses: the unit itself, or for a component the
// units an output read comes from, or all its units for anything else,
// and those that its if and for_each read, which make its instances.
func (c *checker) unitDeps(at *Node, name string, rest []string, seen map[localKey]bool) []string {
	path := name
	if at != nil {
		path = at.Path + "/" + name
	}
	n := c.p.Nodes[path]
	if n == nil {
		return nil
	}
	comp := n.Component()
	if comp == nil {
		return []string{path}
	}
	var out []string
	for _, v := range []any{condition(n.Unit.If), n.Unit.ForEach} {
		out = append(out, c.dataDeps(v, n.In, n.Parent, seen)...)
	}
	if n.Unit.ForEach != nil && len(rest) > 0 {
		rest = rest[1:] // the key of an instance
	}
	switch {
	case len(rest) >= 2 && rest[0] == "outputs":
		if o := comp.Outputs[rest[1]]; o != nil {
			out = append(out, c.dataDeps(o.Value, comp, n, seen)...)
		}
	case len(rest) == 1 && rest[0] == "outputs":
		for _, k := range slices.Sorted(maps.Keys(comp.Outputs)) {
			out = append(out, c.dataDeps(comp.Outputs[k].Value, comp, n, seen)...)
		}
	default:
		for _, l := range slices.Sorted(maps.Keys(c.p.Nodes)) {
			if strings.HasPrefix(l, path+"/") && c.p.Nodes[l].Component() == nil {
				out = append(out, l)
			}
		}
	}
	return out
}
