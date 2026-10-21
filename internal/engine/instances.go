package engine

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
)

// instance is one instance of a unit: the unit itself, or one key of its
// for_each, in the project or in an instance of a component, and what the
// run found and did there.
type instance struct {
	unit      string // the path of the unit, such as web or site/db
	key       string
	keys      []string // those of the instances of the components it is in, outermost first, and its own
	each      any
	eachMarks any    // each.value is sensitive
	address   string // <frame address>/<name>, or <frame address>/<name>[<key>]
	node      *model.Node
	frame     *frame
	def       *model.Unit // the unit as it runs, with what the components it is in give it
	dir       string      // directory of the file that wrote the unit
	deps      []*instance
	backend   string // the backend input of a unit whose plugin has state: tool, as JSON

	record   *state.Record
	plan     plan
	approval string // what approved its change: auto, --yes, or a step
	outcome  string
	changed  bool
	outputs  map[string]any
	outMarks map[string]any // the sensitive outputs
	done     chan struct{}
}

// plan is what the run will do to an instance.
type plan struct {
	run      bool     // up runs its program
	reason   string   // why, when it runs
	waitFor  string   // an upstream unit whose outputs this run changes before this one's turn
	locked   string   // the holder of the address, for another run
	digest   string   // the digest trigger, which the record keeps
	teardown []string // runs whose attempts up tears down after it succeeds
	err      error

	// From the plan of a plugin: its changes, the outputs that change, its
	// own digest, which up gets, and whether it promised to be exact.
	changes      []map[string]any
	outputs      []string
	pluginDigest string
	inexact      bool
}

// approval is the digest that approves the plan: the digest trigger joined
// with that of the plugin's plan.
func (p plan) approval() string { return planDigest(p.digest, p.pluginDigest) }

func (p plan) summary() string {
	old := ""
	if len(p.teardown) > 0 {
		old = "tear down the attempts of " + strings.Join(p.teardown, ", ")
	}
	switch {
	case p.err != nil:
		return "error: " + p.err.Error()
	case p.locked != "":
		return "locked by " + p.locked + "; left out"
	case p.waitFor != "":
		return "planned after " + p.waitFor
	case p.run && old != "":
		return "will run: " + p.reason + "; then " + old
	case p.run:
		return "will run: " + p.reason
	case old != "":
		return "will " + old
	}
	return "unchanged"
}

var forEachKey = regexp.MustCompile(`^[a-z0-9._-]+$`)

// selection is the units a run is about: those named and, unless
// --no-deps, the units they depend on, or for down those that depend on
// them. A name picks by the segments of an address: site picks a unit, or
// every unit inside a component, site[eu] one instance, and site/db a unit
// inside; * matches any text in a segment.
type selection struct {
	all    bool
	picks  map[string][]pick // by the path of a unit that runs
	frames map[string][]pick // the instances of components picked by name, by the path of the unit that uses each
}

// pick is how a unit is picked: patterns of the keys of the components it
// is in and of its own, one per segment of its path, and whether a name
// picked it rather than a dependency.
type pick struct {
	keys  []string
	named bool
}

// selectUnits reads the names and patterns, then follows the graph: a
// dependency inside the same instances of components as the unit that
// needs it is picked in those instances only.
func (e *engine) selectUnits(down bool) error {
	s := &selection{picks: map[string][]pick{}, frames: map[string][]pick{}}
	e.sel = s
	if len(e.pass.units) == 0 {
		s.all = true
		return nil
	}
	type item struct {
		path string
		pick pick
	}
	var todo []item
	seen := map[string]bool{}
	add := func(path string, pk pick) {
		k := path + " " + strings.Join(pk.keys, " ") + map[bool]string{true: " named"}[pk.named]
		if !seen[k] {
			seen[k] = true
			s.picks[path] = append(s.picks[path], pk)
			todo = append(todo, item{path, pk})
		}
	}
	for _, pat := range e.pass.units {
		segs := model.ParseAddress(pat)
		found := false
		for _, path := range slices.Sorted(maps.Keys(e.p.Nodes)) {
			names := strings.Split(path, "/")
			if len(names) != len(segs) {
				continue
			}
			ok := true
			for i, sg := range segs {
				ok = ok && match(sg.Unit, names[i])
			}
			if !ok {
				continue
			}
			found = true
			keysOf := func(p string) []string {
				keys := make([]string, strings.Count(p, "/")+1)
				for i := range keys {
					keys[i] = "*"
					if i < len(segs) && segs[i].Key != "" {
						keys[i] = segs[i].Key
					}
				}
				return keys
			}
			for _, leaf := range e.p.Leaves(path) {
				add(leaf, pick{keys: keysOf(leaf), named: true})
			}
			for _, p := range slices.Sorted(maps.Keys(e.p.Nodes)) {
				if (p == path || strings.HasPrefix(p, path+"/")) && e.p.Nodes[p].Component() != nil {
					s.frames[p] = append(s.frames[p], pick{keys: keysOf(p), named: true})
				}
			}
		}
		if !found {
			return fmt.Errorf("no unit matches %s", pat)
		}
	}
	if e.pass.noDeps {
		return nil
	}
	next := e.p.Deps
	if down {
		next = map[string][]string{}
		for u, deps := range e.p.Deps {
			for _, d := range deps {
				next[d] = append(next[d], u)
			}
		}
	}
	for len(todo) > 0 {
		it := todo[0]
		todo = todo[1:]
		for _, d := range next[it.path] {
			j := sharedDepth(it.path, d)
			keys := make([]string, strings.Count(d, "/")+1)
			for i := range keys {
				keys[i] = "*"
				if i < j {
					keys[i] = it.pick.keys[i]
				}
			}
			add(d, pick{keys: keys})
		}
	}
	return nil
}

// sharedDepth is how many components the units at paths a and b are both
// in.
func sharedDepth(a, b string) int {
	x, y := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(x)-1 && n < len(y)-1 && x[n] == y[n] {
		n++
	}
	return n
}

func match(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

func (s *selection) has(inst *instance) bool { return s.picked(inst.unit, inst.keys, false) }

// named says whether a name picked inst, not a dependency.
func (s *selection) named(inst *instance) bool { return s.picked(inst.unit, inst.keys, true) }

// picked says whether the unit at path, in the instances with keys, is
// picked, by a name when named is set.
func (s *selection) picked(path string, keys []string, named bool) bool {
	if s.all {
		return true
	}
	for _, pk := range s.picks[path] {
		if (!named || pk.named) && pk.matches(keys) {
			return true
		}
	}
	return false
}

// hasFrame says whether a name picked the instance of a component f, whose
// tasks then run.
func (s *selection) hasFrame(f *frame) bool {
	if s.all {
		return true
	}
	for _, pk := range s.frames[f.node.Path] {
		if pk.matches(f.keys()) {
			return true
		}
	}
	return false
}

func (pk pick) matches(keys []string) bool {
	ok := len(pk.keys) == len(keys)
	for i := 0; ok && i < len(keys); i++ {
		ok = match(pk.keys[i], keys[i])
	}
	return ok
}

// expand finds the instances of the unit at path, in every instance of
// the components it is in: if says whether it is there, and for_each makes
// one per key.
func (e *engine) expand(path string) ([]*instance, error) {
	n := e.p.Nodes[path]
	frames, err := e.framesOf(n.Parent)
	if err != nil {
		return nil, err
	}
	def := n.Def()
	var out []*instance
	for _, f := range frames {
		keys, items, eachMarks, err := e.keysOf(f, n)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			id := n.Name
			if n.Unit.ForEach != nil {
				id += "[" + k + "]"
			}
			out = append(out, &instance{unit: path, key: k, keys: append(f.keys(), k), each: items[k], eachMarks: eachMarks, address: f.address + "/" + id,
				node: n, frame: f, def: def, dir: n.Dir(), done: make(chan struct{})})
		}
	}
	return out, nil
}

// keysOf evaluates if and for_each of n in f: no keys when if is false,
// one empty key without for_each, and one per item with it, in key order.
// Both read only what is known when the run starts, so the outputs they
// read come from units this run leaves as they are.
// each.value is sensitive when for_each is; a key never is.
func (e *engine) keysOf(f *frame, n *model.Node) ([]string, map[string]any, any, error) {
	u := n.Unit
	for _, v := range []any{u.If, u.ForEach} {
		if v == nil {
			continue
		}
		if up := e.changing(v, f); up != "" {
			return nil, nil, nil, fmt.Errorf("if and for_each of %s are known when the run starts, but this run changes %s first; run opstruss up %s, then this", n.Path, up, up)
		}
	}
	scope := e.scopeOf(f, n, "", nil, nil)
	if u.If != nil {
		ok, err := expr.EvalCondition(u.If, scope)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("if: %w", err)
		}
		if !ok {
			return nil, nil, nil, nil
		}
	}
	if u.ForEach == nil {
		return []string{""}, nil, nil, nil
	}
	v, marks, err := expr.EvalMarked(u.ForEach, scope)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("for_each: %w", err)
	}
	var eachMarks any
	if marks != nil {
		if _, list := v.([]any); list {
			return nil, nil, nil, errors.New("for_each: a sensitive list would make its items keys, which addresses show; give each item a key of its own with a map")
		}
		e.hide(v, marks)
		eachMarks = map[string]any{"value": true}
	}
	items := map[string]any{}
	switch x := v.(type) {
	case map[string]any:
		items = x
	case []any:
		for _, it := range x {
			k, ok := it.(string)
			if !ok {
				return nil, nil, nil, fmt.Errorf("for_each of a unit takes strings or a map, not %s; build a map with keys of your own", model.TypeName(it))
			}
			if _, dup := items[k]; dup {
				return nil, nil, nil, fmt.Errorf("for_each has %s twice", k)
			}
			items[k] = k
		}
	default:
		return nil, nil, nil, fmt.Errorf("for_each takes a list or a map, not %s", model.TypeName(v))
	}
	keys := slices.Sorted(maps.Keys(items))
	for _, k := range keys {
		if e.redact.Contains(k) {
			return nil, nil, nil, errors.New("for_each: a key holds a sensitive value, and keys show in addresses")
		}
		if !forEachKey.MatchString(k) {
			return nil, nil, nil, fmt.Errorf("for_each key %q has more than lower-case letters, digits, '.', '-', and '_'", k)
		}
	}
	return keys, items, eachMarks, nil
}
