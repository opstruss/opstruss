package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
)

// frame is where units are: the project, or one instance of a component
// that a unit uses. The units inside read its inputs and locals, and the
// unit that uses the component gives each of them its env, as the table of
// keys of a component unit says.
type frame struct {
	node      *model.Node    // the unit that uses the component; nil for the project
	in        *model.Project // the files that write the units inside
	parent    *frame
	key       string
	each      any
	eachMarks any    // each.value is sensitive
	address   string // <stack>, or the address of the instance, such as <stack>/site[eu]
}

// frameSet is the instances of a component unit in a pass, or why they
// could not be found.
type frameSet struct {
	frames []*frame
	err    error
}

// keys are the keys of the instances of the components f is in and of its
// own, outermost first.
func (f *frame) keys() []string {
	var out []string
	for g := f; g.node != nil; g = g.parent {
		out = append(out, g.key)
	}
	slices.Reverse(out)
	return out
}

// child is the path of the unit name in the files of f.
func (f *frame) child(name string) string {
	if f.node == nil {
		return name
	}
	return f.node.Path + "/" + name
}

// rootUnits are the names of the units of the project.
func (e *engine) rootUnits() []string { return slices.Sorted(maps.Keys(e.p.Units)) }

// eachOf is each.key and each.value for a unit with for_each.
func eachOf(u *model.Unit, key string, value any) map[string]any {
	if u.ForEach == nil {
		return nil
	}
	return map[string]any{"key": key, "value": value}
}

// framesOf returns the instances of the component that n uses, in every
// instance of the components around it, found once a pass; for nil, the
// project.
func (e *engine) framesOf(n *model.Node) ([]*frame, error) {
	if n == nil {
		return []*frame{e.top}, nil
	}
	e.mu.Lock()
	fs := e.frames[n.Path]
	e.mu.Unlock()
	if fs != nil {
		return fs.frames, fs.err
	}
	fs = &frameSet{}
	parents, err := e.framesOf(n.Parent)
	for _, pf := range parents {
		keys, items, eachMarks, kerr := e.keysOf(pf, n)
		if kerr != nil {
			err = fmt.Errorf("%s: %w", e.short(pf.address+"/"+n.Name), kerr)
			break
		}
		for _, k := range keys {
			id := n.Name
			if n.Unit.ForEach != nil {
				id += "[" + k + "]"
			}
			fs.frames = append(fs.frames, &frame{node: n, in: n.Component(), parent: pf, key: k, each: items[k], eachMarks: eachMarks,
				address: pf.address + "/" + id})
		}
	}
	if err != nil {
		fs = &frameSet{err: err}
	}
	e.mu.Lock()
	if prev := e.frames[n.Path]; prev != nil {
		fs = prev
	} else {
		e.frames[n.Path] = fs
	}
	collect := e.collect
	e.mu.Unlock()
	if collect && fs.err == nil {
		// Found in the preview of a pass, before any unit inside, which
		// then plans from the records its moves give it.
		for _, g := range fs.frames {
			e.addMoves(g)
		}
	}
	return fs.frames, fs.err
}

// expandComponents finds the instances of every component unit, also of
// those whose components have no unit that runs, once in the pass.
func (e *engine) expandComponents() {
	for _, path := range slices.Sorted(maps.Keys(e.p.Nodes)) {
		if n := e.p.Nodes[path]; n.Component() != nil {
			_, _ = e.framesOf(n)
		}
	}
}

// allFrames are the instances of every component unit that the pass
// found, outer ones first.
func (e *engine) allFrames() []*frame {
	var out []*frame
	for _, path := range slices.Sorted(maps.Keys(e.p.Nodes)) {
		if n := e.p.Nodes[path]; n.Component() != nil {
			fs, _ := e.framesOf(n)
			out = append(out, fs...)
		}
	}
	return out
}

// scope is what the expressions of inst read.
func (e *engine) scope(inst *instance) *expr.Scope {
	return e.scopeOf(inst.frame, inst.node, inst.address, eachOf(inst.node.Unit, inst.key, inst.each), inst.eachMarks)
}

// useScope is what the expressions of the unit that uses the component of
// f read for that instance: its with:, env:, and the like.
func (e *engine) useScope(f *frame) *expr.Scope {
	return e.scopeOf(f.parent, f.node, f.address, eachOf(f.node.Unit, f.key, f.each), f.eachMarks)
}

// scopeOf is what the expressions of unit n in f read: for an instance at
// address with each, or, with an empty address, for its if and for_each,
// which come before its instances.
func (e *engine) scopeOf(f *frame, n *model.Node, address string, each map[string]any, eachMarks any) *expr.Scope {
	return e.frameScope(f, n.Reads, n.Dir(), address, each, eachMarks)
}

// frameScope is what expressions written in the files of f read, with
// units.<name> holding the units in names, and the marks of what is
// sensitive there.
func (e *engine) frameScope(f *frame, names []string, dir, address string, each map[string]any, eachMarks any) *expr.Scope {
	k := e.passKV()
	inputs, inputMarks := e.inputsAndMarks(f)
	facts := e.facts(nil)
	if address != "" {
		facts["address"], facts["dir"] = address, dir
	}
	units, unitMarks := e.unitsView(f, names)
	vars := map[string]any{"stack": e.p.StackName, "inputs": inputs, "locals": k.localsOf(f, inputs), "opstruss": facts,
		"units": units}
	if each != nil {
		vars["each"] = each
	}
	marks := map[string]any{}
	for name, m := range map[string]map[string]any{"inputs": inputMarks, "locals": e.localMarksOf(f, inputMarks, k), "units": unitMarks} {
		if len(m) > 0 {
			marks[name] = m
		}
	}
	if eachMarks != nil {
		marks["each"] = eachMarks
	}
	return (&expr.Scope{Dir: dir, Vars: vars, Marks: marks}).WithKV(k.get, k.marked)
}

// inputsOf is inputs.<name> in f: those of the project, or those of the
// component, from what the unit that uses it gives in with:, then env:,
// then default:, each evaluated now. An input that has no value yet, as
// one whose unit upstream runs later, fails only the expressions that read
// it.
func (e *engine) inputsOf(f *frame) map[string]any {
	inputs, _ := e.inputsAndMarks(f)
	return inputs
}

// inputsAndMarks is inputsOf with the marks of the sensitive inputs: those
// that the project or component declares so, and those that the unit that
// uses the component gives from a sensitive value.
func (e *engine) inputsAndMarks(f *frame) (map[string]any, map[string]any) {
	if f.node == nil {
		return e.inputs, e.inputMarks
	}
	s := e.useScope(f)
	out, marks := map[string]any{}, map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(f.in.Inputs)) {
		in := f.in.Inputs[name]
		var v any
		var err error
		if w, ok := f.node.Unit.With[name]; ok {
			var m any
			if v, m, err = expr.EvalMarked(w, s); err == nil && m != nil {
				e.hide(v, true)
				marks[name] = true
			}
		}
		if err == nil && v == nil && in.Env != "" {
			if text, ok := e.opts.LookupEnv(in.Env); ok {
				if in.Sensitive {
					e.known("input "+name+" of "+e.short(f.address), text)
				}
				v, err = model.ParseText(text, in.Type)
			}
		}
		if in.Sensitive {
			marks[name] = true
			if err == nil && v != nil {
				e.known("input "+name+" of "+e.short(f.address), v)
			}
		}
		if err == nil {
			v, err = in.Conform(v)
		}
		switch {
		case err != nil:
			out[name] = expr.Broken(fmt.Errorf("input %s of %s: %w", name, e.short(f.address), err))
		case v != nil:
			out[name] = v
		}
	}
	return out, marks
}

// localMarksOf marks the sensitive locals in f, which read a sensitive
// input, local, or value of kv() in k.
func (e *engine) localMarksOf(f *frame, inputMarks map[string]any, k *kvMemory) map[string]any {
	if f.node == nil && !e.kvLocals {
		return e.localMarks
	}
	return f.in.LocalMarks(map[string]any{"inputs": inputMarks}, k.marked)
}

// frameEnv adds to env the env of the units that use the components f is
// in, outermost first, each evaluated where it is written.
// The names of the sensitive ones go to hidden.
func (e *engine) frameEnv(f *frame, env map[string]model.EnvValue, hidden map[string]bool) error {
	if f.node == nil {
		return nil
	}
	if err := e.frameEnv(f.parent, env, hidden); err != nil {
		return err
	}
	s := e.useScope(f)
	for k, v := range f.node.Unit.Env {
		text, sensitive, err := expr.EvalTextMarked(v.Text, s)
		if err != nil {
			return fmt.Errorf("env.%s of %s: %w", k, e.short(f.address), err)
		}
		env[k] = model.EnvValue{Text: text, File: v.File}
		hidden[k] = sensitive
		if sensitive {
			e.hide(text, true)
		}
	}
	return nil
}

// unitsView is units.<name> in f for the units in names: the outputs of
// this run for the units it ran, and the stored ones for the others, with
// outcome, changed, and plan once a pass of this run reached them; a unit
// with for_each is a map of its instances, and a unit that uses a
// component holds what the component gives.
// The marks say which outputs are sensitive.
func (e *engine) unitsView(f *frame, names []string) (map[string]any, map[string]any) {
	views, viewMarks := map[string]any{}, map[string]map[string]any{}
	var comps []*model.Node
	e.mu.Lock()
	for _, name := range names {
		n := e.p.Nodes[f.child(name)]
		switch {
		case n == nil:
		case n.Component() != nil:
			comps = append(comps, n)
		default:
			insts, ok := e.byUnit[n.Path]
			if !ok {
				continue
			}
			byKey, marks := map[string]any{}, map[string]any{}
			for _, inst := range insts {
				if inst.frame == f {
					byKey[inst.key] = inst.view()
					if m := inst.viewMarks(); m != nil {
						marks[inst.key] = m
					}
				}
			}
			views[name], viewMarks[name] = byKey, marks
		}
	}
	inPass := e.inPass
	e.mu.Unlock()
	for _, n := range comps {
		frames, err := e.framesOf(n)
		if err != nil {
			views[n.Name] = expr.Broken(err)
			continue
		}
		byKey, marks := map[string]any{}, map[string]any{}
		for _, g := range frames {
			if g.parent == f {
				var m any
				byKey[g.key], m = e.componentView(g)
				if m != nil {
					marks[g.key] = m
				}
			}
		}
		views[n.Name], viewMarks[n.Name] = byKey, marks
	}
	// Outside a pass, as in a step of a task, units that no pass of this run
	// expanded read their records. Inside one, units are expanded in graph
	// order, so the units an expression may read are expanded already.
	if !inPass {
		var addrs []string
		for _, name := range names {
			if _, ok := views[name]; ok || e.p.Nodes[f.child(name)] == nil {
				continue
			}
			if addrs == nil {
				addrs, _ = e.store.Inventory()
			}
			byKey, marks := map[string]any{}, map[string]any{}
			for _, a := range addrs {
				rest, ok := strings.CutPrefix(a, f.address+"/")
				if !ok || strings.Contains(rest, "/") {
					continue
				}
				seg := model.ParseAddress(rest)[0]
				if seg.Unit != name {
					continue
				}
				if rec, _, err := e.store.Get(a); err == nil && rec != nil && rec.Tombstone == nil {
					out := rec.Outputs
					if out == nil {
						out = map[string]any{}
					}
					byKey[seg.Key] = map[string]any{"outputs": out}
					if m := hiddenMarks(rec.Hidden[state.PartOutputs]); m != nil {
						marks[seg.Key] = map[string]any{"outputs": m}
					}
				}
			}
			views[name], viewMarks[name] = byKey, marks
		}
	}
	view, marks := map[string]any{}, map[string]any{}
	for name, v := range views {
		byKey, ok := v.(map[string]any)
		switch {
		case !ok:
			view[name] = v
		case e.p.Nodes[f.child(name)].Unit.ForEach != nil:
			view[name] = byKey
			if len(viewMarks[name]) > 0 {
				marks[name] = viewMarks[name]
			}
		case byKey[""] != nil:
			view[name] = byKey[""]
			if m := viewMarks[name][""]; m != nil {
				marks[name] = m
			}
		default:
			view[name] = map[string]any{"outputs": map[string]any{}}
		}
	}
	return view, marks
}

// hiddenMarks are the marks of the values names, as a record hides them.
func hiddenMarks(names []string) map[string]any {
	if len(names) == 0 {
		return nil
	}
	m := map[string]any{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// viewMarks are the marks of view(): its sensitive outputs.
func (inst *instance) viewMarks() any {
	if m := inst.outputMarksNow(); len(m) > 0 {
		return map[string]any{"outputs": m}
	}
	return nil
}

// view is units.<name> of one instance; the caller holds e.mu.
func (inst *instance) view() map[string]any {
	v := map[string]any{"outputs": inst.outputsNow()}
	if inst.outcome != "" {
		v["outcome"], v["changed"] = inst.outcome, inst.changed
	}
	if inst.plan.digest != "" || inst.plan.waitFor != "" {
		v["plan"] = inst.plan.view(inst.plan.err == nil && inst.plan.waitFor == "" && inst.plan.locked == "")
	}
	return v
}

// componentView is units.<name> of a unit that uses a component, for its
// instance g: the component's outputs, and once a pass reached the units
// inside, their outcome and changed together, and their plans as one, as
// a module is in the JSON plan of Terraform. The units inside stay out of
// reach.
// The marks say which outputs are sensitive: declared so, or read from a
// sensitive value.
func (e *engine) componentView(g *frame) (map[string]any, any) {
	s := e.frameScope(g, slices.Sorted(maps.Keys(g.in.Units)), g.in.Dir, g.address, nil, nil)
	outputs, outMarks := map[string]any{}, map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(g.in.Outputs)) {
		so := *s
		so.Dir = g.in.DirOf("outputs." + k)
		v, m, err := expr.EvalMarked(g.in.Outputs[k].Value, &so)
		if err != nil {
			v = expr.Broken(fmt.Errorf("output %s of %s: %w", k, e.short(g.address), err))
		}
		outputs[k] = v
		if m != nil || g.in.Outputs[k].Sensitive {
			outMarks[k] = true
			e.hide(v, true)
		}
	}
	var marks any
	if len(outMarks) > 0 {
		marks = map[string]any{"outputs": outMarks}
	}
	view := map[string]any{"outputs": outputs}
	rank := map[string]int{success: 0, skipped: 1, cancelled: 2, failure: 3}
	e.mu.Lock()
	defer e.mu.Unlock()
	outcome, changed, reached, all := success, false, 0, true
	var planned []*instance
	for _, inst := range e.inside(g) {
		if inst.outcome == "" {
			all = false
		} else {
			reached++
			changed = changed || inst.changed
			if rank[inst.outcome] > rank[outcome] {
				outcome = inst.outcome
			}
		}
		if inst.plan.digest != "" || inst.plan.waitFor != "" {
			planned = append(planned, inst)
		}
	}
	if reached > 0 && all {
		view["outcome"], view["changed"] = outcome, changed
	}
	if len(planned) > 0 {
		changes := []any{}
		exact := true
		var lines, digests []string
		for _, inst := range planned {
			v := inst.view()["plan"].(map[string]any)
			short := e.short(inst.address)
			for _, c := range v["changes"].([]any) {
				m := maps.Clone(c.(map[string]any))
				m["address"] = short
				changes = append(changes, m)
			}
			exact = exact && v["exact"].(bool)
			lines = append(lines, short+": "+v["summary"].(string))
			digests = append(digests, short+" "+v["digest"].(string))
		}
		sum := sha256.Sum256([]byte(strings.Join(digests, "\n")))
		view["plan"] = map[string]any{"changes": changes, "outputs": []any{}, "exact": exact,
			"digest": fmt.Sprintf("sha256:v1:%x", sum), "summary": strings.Join(lines, "\n")}
	}
	return view, marks
}

// inside are the instances of the units that run inside g, also in the
// components inside it; the caller holds e.mu.
func (e *engine) inside(g *frame) []*instance {
	prefix := g.keys()
	var out []*instance
	for _, path := range e.p.Leaves(g.node.Path) {
		for _, inst := range e.byUnit[path] {
			if len(inst.keys) > len(prefix) && slices.Equal(inst.keys[:len(prefix)], prefix) {
				out = append(out, inst)
			}
		}
	}
	return out
}

// changing returns the first unit whose outputs v, written in the files of
// f, reads and that this run changes or plans later: read directly, or
// through an input of the component, a local, or an output of a component.
func (e *engine) changing(v any, f *frame) string {
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		return ""
	}
	for _, r := range use.Refs {
		var up string
		switch r.Path[0] {
		case "units":
			if len(r.Path) > 1 {
				up = e.changingUnit(f, r.Path[1], r.Path[2:])
			}
		case "inputs":
			if f.node == nil {
				continue
			}
			for _, k := range slices.Sorted(maps.Keys(f.node.Unit.With)) {
				if up == "" && (len(r.Path) == 1 || r.Path[1] == k) {
					up = e.changing(f.node.Unit.With[k], f.parent)
				}
			}
		case "locals":
			for _, k := range slices.Sorted(maps.Keys(f.in.Locals)) {
				if up == "" && (len(r.Path) == 1 || r.Path[1] == k) {
					up = e.changing(f.in.Locals[k], f)
				}
			}
		}
		if up != "" {
			return up
		}
	}
	return ""
}

// changingUnit is changing for units.<name><rest> read in f.
func (e *engine) changingUnit(f *frame, name string, rest []string) string {
	n := e.p.Nodes[f.child(name)]
	if n == nil {
		return ""
	}
	comp := n.Component()
	if comp == nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, up := range e.byUnit[n.Path] {
			if up.frame == f && (up.plan.run || up.plan.waitFor != "") && e.sel.has(up) {
				return e.short(up.address)
			}
		}
		return ""
	}
	frames, _ := e.framesOf(n)
	if n.Unit.ForEach != nil && len(rest) > 0 {
		rest = rest[1:] // the key of an instance
	}
	for _, g := range frames {
		if g.parent != f {
			continue
		}
		up := ""
		switch {
		case len(rest) >= 2 && rest[0] == "outputs":
			if o := comp.Outputs[rest[1]]; o != nil {
				up = e.changing(o.Value, g)
			}
		case len(rest) == 1 && rest[0] == "outputs":
			for _, k := range slices.Sorted(maps.Keys(comp.Outputs)) {
				if up == "" {
					up = e.changing(comp.Outputs[k].Value, g)
				}
			}
		default:
			e.mu.Lock()
			for _, inst := range e.inside(g) {
				if up == "" && (inst.plan.run || inst.plan.waitFor != "") && e.sel.has(inst) {
					up = e.short(inst.address)
				}
			}
			e.mu.Unlock()
		}
		if up != "" {
			return up
		}
	}
	return ""
}

// depsOf are the instances of the unit at path d that inst depends on: in
// the components both are in, those in the same instances, and every
// instance in the rest.
func (e *engine) depsOf(inst *instance, d string) []*instance {
	j := sharedDepth(inst.unit, d)
	var out []*instance
	for _, di := range e.byUnit[d] {
		if slices.Equal(di.keys[:j], inst.keys[:j]) {
			out = append(out, di)
		}
	}
	return out
}

// holdFrames writes the record of each instance of a component that inst
// is in, outermost first, when it has none or one with another source or
// inputs, each under its lock; release lets the locks go once inst wrote
// its own record, so the record of an instance never goes while a unit
// inside writes its first one.
func (e *engine) holdFrames(ctx context.Context, inst *instance) (release func(), err error) {
	var chain []*frame
	for f := inst.frame; f.node != nil; f = f.parent {
		chain = append(chain, f)
	}
	slices.Reverse(chain)
	var held [][2]string
	release = func() {
		for i := len(held) - 1; i >= 0; i-- {
			_ = e.store.Unlock(held[i][0], held[i][1])
		}
	}
	for _, f := range chain {
		token, err := e.briefLock(ctx, f.address)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, [2]string{f.address, token})
		if err := e.writeFrame(f); err != nil {
			release()
			return nil, fmt.Errorf("the record of %s: %w", e.short(f.address), err)
		}
	}
	return release, nil
}

// writeFrame writes the record of the component instance f under its
// lock: its source and inputs, and no down, since the units inside have
// records of their own.
func (e *engine) writeFrame(f *frame) error {
	rec, etag, err := e.store.Get(f.address)
	if err == nil && rec != nil && rec.Tombstone != nil {
		err = tombstoned(rec)
	}
	if err == nil && rec != nil && rec.Tombstone == nil && rec.Kind != "component" {
		err = fmt.Errorf("it holds a unit of another kind; take that unit down with the files that still have it first")
	}
	if err != nil {
		return err
	}
	inputs, hidden := map[string]any{}, []string(nil)
	all, marks := e.inputsAndMarks(f)
	for k, v := range all {
		if !expr.IsBroken(v) {
			inputs[k] = v
			if marks[k] != nil {
				hidden = append(hidden, k)
			}
		}
	}
	slices.Sort(hidden)
	src := &state.Source{Uses: f.node.Unit.Uses}
	if rec != nil && same(rec.Source, src) && same(rec.Inputs, inputs) {
		return nil
	}
	next := &state.Record{Kind: "component", Source: src, Inputs: inputs, Run: e.runID, Updated: time.Now().UTC().Format(time.RFC3339)}
	if len(hidden) > 0 {
		next.Hidden = map[string][]string{state.PartInputs: hidden}
	}
	if rec != nil {
		next.ID = rec.ID
	} else {
		next.ID = state.NewID()
	}
	_, err = e.store.Put(f.address, next, etag)
	return err
}

// frameAddresses are the addresses of the instances of components that
// addr is in, innermost first.
func (e *engine) frameAddresses(addr string) []string {
	var out []string
	for i := strings.LastIndex(addr, "/"); i > len(e.p.StackName); i = strings.LastIndex(addr[:i], "/") {
		out = append(out, addr[:i])
	}
	return out
}

// dropFrames removes the record of each component instance at addrs once
// no unit inside has a record, innermost first, each under its lock.
func (e *engine) dropFrames(ctx context.Context, addrs []string) {
	slices.SortFunc(addrs, func(a, b string) int {
		return cmp.Or(strings.Count(b, "/")-strings.Count(a, "/"), strings.Compare(a, b))
	})
	for _, addr := range slices.Compact(addrs) {
		if err := e.dropFrame(ctx, addr); err != nil {
			e.warn(addr, "keeping the record of the instance: %v", err)
		}
	}
}

func (e *engine) dropFrame(ctx context.Context, addr string) error {
	token, err := e.briefLock(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = e.store.Unlock(addr, token) }()
	rec, etag, err := e.store.Get(addr)
	if err != nil || rec == nil || rec.Kind != "component" || rec.Tombstone != nil {
		return err
	}
	addrs, err := e.store.Inventory()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if !strings.HasPrefix(a, addr+"/") {
			continue
		}
		if r, _, err := e.store.Get(a); err != nil || r != nil && r.Tombstone == nil {
			return err
		}
	}
	if err := e.store.Delete(addr, etag); err != nil {
		return err
	}
	e.say("unit", addr, "the instance has no units left; its record is gone")
	return nil
}
