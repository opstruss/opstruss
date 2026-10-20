package engine

import (
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
// for_each, and what the run found and did there.
type instance struct {
	unit    string
	key     string
	each    any
	id      string // name, or name[key]
	address string // <stack>/<id>
	def     *model.Unit
	dir     string // directory of the file that wrote the unit
	deps    []*instance

	record  *state.Record
	plan    plan
	outcome string
	changed bool
	outputs map[string]any
	done    chan struct{}
}

// plan is what the run will do to an instance.
type plan struct {
	run     bool   // up runs its program
	reason  string // why, when it runs
	waitFor string // an upstream unit whose outputs this run changes before this one's turn
	locked  string // the holder of the address, for another run
	digest  string
	err     error
}

func (p plan) summary() string {
	switch {
	case p.err != nil:
		return "error: " + p.err.Error()
	case p.locked != "":
		return "locked by " + p.locked + "; left out"
	case p.waitFor != "":
		return "planned after " + p.waitFor
	case p.run:
		return "will run: " + p.reason
	}
	return "unchanged"
}

var forEachKey = regexp.MustCompile(`^[a-z0-9._-]+$`)

// selection is the units a run is about: those named and, unless
// --no-deps, the units they depend on, or for down those that depend on
// them.
type selection struct {
	all   bool
	units map[string]bool
	keys  map[string][]string // patterns of the keys named, for a unit named as name[key]
	named map[string]bool     // units named rather than reached as dependencies
}

// selectUnits reads the names and patterns: * matches any text, and
// name[key] picks one instance.
func (e *engine) selectUnits(down bool) error {
	s := &selection{units: map[string]bool{}, keys: map[string][]string{}, named: map[string]bool{}}
	e.sel = s
	if len(e.opts.Units) == 0 {
		s.all = true
		return nil
	}
	for _, pat := range e.opts.Units {
		name, key, hasKey := strings.Cut(strings.TrimSuffix(pat, "]"), "[")
		found := false
		for _, u := range slices.Sorted(maps.Keys(e.p.Units)) {
			if !match(name, u) {
				continue
			}
			found = true
			s.units[u], s.named[u] = true, true
			if hasKey {
				s.keys[u] = append(s.keys[u], key)
			}
		}
		if !found {
			return fmt.Errorf("no unit matches %s", pat)
		}
	}
	if e.opts.NoDeps {
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
	for todo := slices.Collect(maps.Keys(s.units)); len(todo) > 0; {
		u := todo[0]
		todo = todo[1:]
		for _, d := range next[u] {
			if !s.units[d] {
				s.units[d] = true
				todo = append(todo, d)
			}
		}
	}
	return nil
}

func match(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

func (s *selection) has(inst *instance) bool {
	if s.all {
		return true
	}
	if !s.units[inst.unit] {
		return false
	}
	keys := s.keys[inst.unit]
	return len(keys) == 0 || slices.ContainsFunc(keys, func(k string) bool { return match(k, inst.key) })
}

// expand finds the instances of unit name: if says whether it is there,
// and for_each makes one per key. Both read only what is known when the
// run starts, so the outputs they read come from units this run leaves as
// they are.
func (e *engine) expand(name string) ([]*instance, error) {
	u := e.p.Units[name]
	dir := e.p.DirOf("units." + name)
	for _, v := range []any{u.If, u.ForEach} {
		if v == nil {
			continue
		}
		if up := e.changing(v); up != "" {
			return nil, fmt.Errorf("if and for_each of %s are known when the run starts, but this run changes %s first; run opstruss up %s, then this", name, up, up)
		}
	}
	scope := e.scope(nil, dir)
	if u.If != nil {
		ok, err := expr.EvalCondition(u.If, scope)
		if err != nil {
			return nil, fmt.Errorf("if: %w", err)
		}
		if !ok {
			return nil, nil
		}
	}
	addr := e.p.StackName + "/" + name
	if u.ForEach == nil {
		return []*instance{{unit: name, id: name, address: addr, def: u, dir: dir, done: make(chan struct{})}}, nil
	}
	v, err := expr.EvalValue(u.ForEach, scope)
	if err != nil {
		return nil, fmt.Errorf("for_each: %w", err)
	}
	items := map[string]any{}
	switch x := v.(type) {
	case map[string]any:
		items = x
	case []any:
		for _, it := range x {
			k, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("for_each of a unit takes strings or a map, not %s; build a map with keys of your own", model.TypeName(it))
			}
			if _, dup := items[k]; dup {
				return nil, fmt.Errorf("for_each has %s twice", k)
			}
			items[k] = k
		}
	default:
		return nil, fmt.Errorf("for_each takes a list or a map, not %s", model.TypeName(v))
	}
	var out []*instance
	for _, k := range slices.Sorted(maps.Keys(items)) {
		if !forEachKey.MatchString(k) {
			return nil, fmt.Errorf("for_each key %q has more than lower-case letters, digits, '.', '-', and '_'", k)
		}
		id := name + "[" + k + "]"
		out = append(out, &instance{unit: name, key: k, each: items[k], id: id, address: e.p.StackName + "/" + id,
			def: u, dir: dir, done: make(chan struct{})})
	}
	return out, nil
}

// changing returns the first unit whose outputs v reads and that this run
// changes or plans later.
func (e *engine) changing(v any) string {
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		return ""
	}
	for _, r := range use.Refs {
		if len(r.Path) < 2 || r.Path[0] != "units" {
			continue
		}
		for _, up := range e.byUnit[r.Path[1]] {
			if (up.plan.run || up.plan.waitFor != "") && e.sel.has(up) {
				return up.unit
			}
		}
	}
	return ""
}

// scope is what the expressions of a unit read: inst is nil for its if and
// for_each.
func (e *engine) scope(inst *instance, dir string) *expr.Scope {
	vars := map[string]any{"stack": e.p.StackName, "inputs": e.inputs, "locals": e.locals,
		"opstruss": e.facts(inst), "units": e.unitsView()}
	if inst != nil && inst.def.ForEach != nil {
		vars["each"] = map[string]any{"key": inst.key, "value": inst.each}
	}
	return &expr.Scope{Dir: dir, Vars: vars}
}

// unitsView is units.<name>: the outputs of this run for the units it ran,
// and the stored ones for the others; a unit with for_each is a map of its
// instances.
func (e *engine) unitsView() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	view := map[string]any{}
	for name, insts := range e.byUnit {
		byKey := map[string]any{}
		for _, inst := range insts {
			out := inst.outputs
			if out == nil && inst.record != nil && inst.record.Tombstone == nil {
				out = inst.record.Outputs
			}
			if out == nil {
				out = map[string]any{}
			}
			byKey[inst.key] = map[string]any{"outputs": out}
		}
		if e.p.Units[name].ForEach == nil {
			view[name] = byKey[""]
		} else {
			view[name] = byKey
		}
	}
	return view
}
