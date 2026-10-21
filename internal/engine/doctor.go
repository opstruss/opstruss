package engine

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/state"
)

// Report is what doctor finds in a stack: whether a run of it has what it
// needs on this machine, check by check. Doctor never resolves a secret,
// since a source may ask for a login or change something.
type Report struct {
	Stack  string   `json:"stack"`
	Checks []*Check `json:"checks"`
}

// Check is one finding: ok; skip when only a run can check it; warn for
// what may matter; error for what fails a run.
type Check struct {
	Check   string   `json:"check"` // program, sops, key, env, or state
	Status  string   `json:"status"`
	Message string   `json:"message"`
	Name    string   `json:"name,omitempty"` // the program or the variable
	Path    string   `json:"path,omitempty"` // where the program is
	For     []string `json:"for,omitempty"`  // what needs it, or asks for it
}

// Failed reports whether a check found what fails a run.
func (r *Report) Failed() bool {
	return slices.ContainsFunc(r.Checks, func(c *Check) bool { return c.Status == "error" })
}

// Doctor checks what a run of the stack needs on this machine: the
// programs of requires:, sops and the data key, the variables that
// plugins ask for, and where .opstruss is.
func Doctor(opts Options) (*Report, error) {
	e, err := e0(opts)
	if err != nil {
		return nil, err
	}
	d := &doctor{e: e, r: &Report{Stack: e.p.StackName, Checks: []*Check{}}, need: map[string][]string{},
		passes: map[*model.Package][]string{}}
	d.units()
	d.tasks()
	d.programs()
	d.key()
	d.environment()
	d.state()
	return d.r, nil
}

type doctor struct {
	e      *engine
	r      *Report
	need   map[string][]string // the programs of requires:, with what needs each
	skip   []*Check            // requires: that only a run can compute
	passes map[*model.Package][]string
}

func (d *doctor) add(c *Check) { d.r.Checks = append(d.r.Checks, c) }

// note notes that what needs the programs names.
func (d *doctor) note(what string, names ...string) {
	for _, n := range names {
		if !slices.Contains(d.need[n], what) {
			d.need[n] = append(d.need[n], what)
		}
	}
}

// requires notes the programs that r names for what, evaluated in s. Those
// of a task that read its inputs come from each call, and a run checks
// them, as it does those it cannot compute here; for a unit, that fails it.
func (d *doctor) requires(what string, r *model.Requires, s *expr.Scope, task bool) {
	if r == nil {
		return
	}
	if task {
		if use, err := expr.AnalyzeValue(r.Value()); err == nil && slices.ContainsFunc(use.Refs, func(ref expr.Ref) bool {
			return ref.Path[0] == "inputs" || ref.Path[0] == "each"
		}) {
			d.skip = append(d.skip, &Check{Check: "program", Status: "skip", For: []string{what},
				Message: fmt.Sprintf("the programs that %s requires depend on its inputs, so each call checks them", what)})
			return
		}
	}
	names, err := requires(r, s)
	switch {
	case err != nil && task:
		d.skip = append(d.skip, &Check{Check: "program", Status: "skip", For: []string{what},
			Message: fmt.Sprintf("the programs that %s requires are known only when it runs: %v", what, err)})
	case err != nil:
		d.add(&Check{Check: "program", Status: "error", For: []string{what}, Message: fmt.Sprintf("requires: of %s: %v", what, err)})
	default:
		d.note(what, names...)
	}
}

// units notes the programs each instance of a unit requires: its own, and
// those of the plugin or the task it uses, with its with: as their inputs.
func (d *doctor) units() {
	for _, inst := range d.e.insts {
		what := "unit " + inst.address
		s := d.e.scope(inst)
		d.requires(what, inst.def.Requires, s, false)
		t := inst.def.Target
		if t == nil {
			continue
		}
		var task *model.Task
		switch t.Kind {
		case model.TargetPlugin:
			task = t.Package.Plugin.Tasks["up"]
		case model.TargetTask:
			task = t.Package.Plugin.Tasks[t.Task]
		case model.TargetProject:
			task = inst.frame.in.Tasks[t.Task]
		}
		if t.Package != nil && t.Package.Plugin != nil {
			d.note(what, t.Package.Plugin.Requires...)
			d.passes[t.Package] = append(d.passes[t.Package], inst.def.Pass...)
		}
		if task == nil || task.Requires == nil {
			continue
		}
		given := map[string]any{}
		if t.Package != nil && t.Package.Decl != nil {
			maps.Copy(given, t.Package.Decl.With)
		}
		maps.Copy(given, inst.def.With)
		v, err := expr.EvalValue(given, s)
		var inputs map[string]any
		if err == nil {
			decl, _ := t.Inputs()
			inputs, err = conform(decl, v.(map[string]any))
		}
		if err != nil {
			d.skip = append(d.skip, &Check{Check: "program", Status: "skip", For: []string{what},
				Message: fmt.Sprintf("the programs that %s requires depend on its with:, which only a run can compute: %v", what, err)})
			continue
		}
		d.requires(what, task.Requires, &expr.Scope{Dir: s.Dir, Vars: map[string]any{"inputs": inputs, "stack": d.e.p.StackName,
			"each": s.Vars["each"], "opstruss": d.e.facts(inst)}}, false)
	}
}

// tasks notes the programs that tasks require: those of the project and of
// each instance of a component, the tasks of units, and the tasks of
// plugins that they and secrets call by name.
func (d *doctor) tasks() {
	e := d.e
	for _, f := range append([]*frame{e.top}, e.allFrames()...) {
		for _, name := range slices.Sorted(maps.Keys(f.in.Tasks)) {
			what := "task " + name
			if f.node != nil {
				what += " of " + f.address
			}
			d.task(what, f.in, f.in.Tasks[name], e.frameScope(f, nil, f.in.DirOf("tasks."+name), "", nil, nil))
		}
	}
	for _, inst := range e.insts {
		for _, name := range slices.Sorted(maps.Keys(inst.def.Tasks)) {
			d.task("task "+name+" of "+inst.address, inst.frame.in, inst.def.Tasks[name], e.scope(inst))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(e.p.Secrets)) {
		src := e.p.Secrets[name]
		for callee := range src.Calls {
			d.call("secret "+name, e.p, callee, src.Pass)
		}
	}
}

// task notes what t requires for what, and what the tasks of plugins it
// calls by name require.
func (d *doctor) task(what string, in *model.Project, t *model.Task, s *expr.Scope) {
	d.requires(what, t.Requires, s, true)
	for _, callee := range model.CallNames(t) {
		d.call(what, in, callee, model.PassesOf(t))
	}
}

// call notes what a call of a task of a plugin requires, and the pass:
// patterns that reach the plugin.
func (d *doctor) call(what string, in *model.Project, callee string, passes []string) {
	t, err := in.Resolve(callee)
	if err != nil || t.Kind != model.TargetTask {
		return
	}
	d.note(what, t.Package.Plugin.Requires...)
	d.passes[t.Package] = append(d.passes[t.Package], passes...)
	d.requires(what, t.Package.Plugin.Tasks[t.Task].Requires, &expr.Scope{Dir: t.Package.Dir,
		Vars: map[string]any{"stack": d.e.p.StackName, "opstruss": d.e.facts(nil)}}, true)
}

// programs checks that each program that requires: names is in PATH.
func (d *doctor) programs() {
	for _, name := range slices.Sorted(maps.Keys(d.need)) {
		what := d.need[name]
		slices.Sort(what)
		c := &Check{Check: "program", Name: name, For: what}
		if p, err := plugin.LookPath(name, d.e.path()); err == nil {
			c.Status, c.Path, c.Message = "ok", p, fmt.Sprintf("%s at %s, for %s", name, p, strings.Join(what, ", "))
		} else {
			c.Status, c.Message = "error", fmt.Sprintf("%s is not in PATH, for %s", name, strings.Join(what, ", "))
		}
		d.add(c)
	}
	d.r.Checks = append(d.r.Checks, d.skip...)
}

// key checks sops and the data key of the stack, when the stack needs
// one: it has one already, or the project has secrets or sensitive
// values. Secrets from sops files need sops too.
func (d *doctor) key() {
	e := d.e
	data, _, err := e.store.MetaData()
	if err != nil {
		d.add(&Check{Check: "key", Status: "error", Message: fmt.Sprintf("reading %s: %v", state.MetaKey(e.p.StackName), err)})
		return
	}
	var hasKey bool
	if data != nil {
		m, err := state.DecodeMeta(data)
		if err != nil {
			d.add(&Check{Check: "key", Status: "error", Message: fmt.Sprintf("reading %s: %v", state.MetaKey(e.p.StackName), err)})
			return
		}
		hasKey = m.Key != ""
	}
	needKey := hasKey || e.p.HasSensitive()
	fromFiles := slices.ContainsFunc(slices.Collect(maps.Values(e.p.Secrets)), func(s *model.Secret) bool { return s.Sops != "" })
	if !needKey && !fromFiles {
		return
	}
	sopsOK := d.sops()
	meta := state.MetaKey(e.p.StackName)
	switch {
	case !needKey:
	case hasKey && !sopsOK:
		// The check of sops says why the key cannot open.
	case hasKey:
		sops := &secret.Sops{Dir: e.p.Dir, Environ: e.opts.Environ, Stack: e.p.StackName}
		if _, _, err := unwrapKey(sops, data); err != nil {
			d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf("the data key of stack %s does not open: %v", e.p.StackName, err)})
		} else {
			d.add(&Check{Check: "key", Status: "ok", Name: meta, Message: fmt.Sprintf("the data key of stack %s opens with sops", e.p.StackName)})
		}
	default:
		ok, err := secret.HasRule(e.p.Dir, secret.MetaName(e.p.StackName))
		switch {
		case err != nil:
			d.add(&Check{Check: "key", Status: "error", Name: meta, Message: err.Error()})
		case !ok:
			d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf(
				"no rule in %s matches %s, so the stack cannot make the data key its sensitive values need; opstruss init adds one", secret.SopsConfig, meta)})
		default:
			d.add(&Check{Check: "key", Status: "ok", Name: meta, Message: fmt.Sprintf(
				"stack %s has no data key yet; the first value that needs one makes it with the rule in %s for %s", e.p.StackName, secret.SopsConfig, meta)})
		}
	}
}

// sops checks that sops is in PATH and new enough.
func (d *doctor) sops() bool {
	p, err := plugin.LookPath("sops", d.e.path())
	if err != nil {
		d.add(&Check{Check: "sops", Status: "error", Name: "sops", Message: fmt.Sprintf(
			"sops is not in PATH, and the stack needs it for its data key or its secrets; install %s or newer, for example with mise use sops", secret.MinSops)})
		return false
	}
	v, old, err := secret.SopsVersion(p, d.e.opts.Environ)
	switch {
	case err != nil:
		d.add(&Check{Check: "sops", Status: "error", Name: "sops", Path: p, Message: fmt.Sprintf("%v; OpsTruss needs sops %s or newer", err, secret.MinSops)})
	case old:
		d.add(&Check{Check: "sops", Status: "error", Name: "sops", Path: p, Message: fmt.Sprintf("sops %s at %s is older than %s, which OpsTruss needs", v, p, secret.MinSops)})
	default:
		d.add(&Check{Check: "sops", Status: "ok", Name: "sops", Path: p, Message: fmt.Sprintf("sops %s at %s", v, p)})
		return true
	}
	return false
}

// environment warns about the variables that a plugin asks for and that
// no pass: of what uses it lets through, so its programs never get them.
func (d *doctor) environment() {
	pkgs := slices.SortedFunc(maps.Keys(d.passes), func(a, b *model.Package) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Dir, b.Dir))
	})
	for _, pkg := range pkgs {
		passes := d.passes[pkg]
		if pkg.Decl != nil {
			passes = append(passes, pkg.Decl.Pass...)
		}
		for _, v := range pkg.Plugin.Env {
			if slices.ContainsFunc(passes, func(p string) bool { ok, _ := path.Match(p, v); return ok }) {
				continue
			}
			d.add(&Check{Check: "env", Status: "warn", Name: v, For: []string{"plugin " + pkg.Name}, Message: fmt.Sprintf(
				"plugin %s asks for %s, and no pass: of what uses it lets it through, so its programs never get it", pkg.Name, v)})
		}
	}
}

// state warns when .opstruss is on a network filesystem, where flock may
// not lock between machines.
func (d *doctor) state() {
	dir, name := d.e.root, ".opstruss"
	if _, err := os.Stat(dir); err != nil {
		dir, name = d.e.p.Dir, "the project"
	}
	fs, network, err := state.NetworkFS(dir)
	switch {
	case err != nil:
		d.add(&Check{Check: "state", Status: "warn", Message: fmt.Sprintf("the filesystem of %s: %v", filepath.Base(dir), err)})
	case network:
		d.add(&Check{Check: "state", Status: "warn", Name: fs, Message: fmt.Sprintf(
			"%s is on a network filesystem (%s), where its locks may not hold between machines; keep the project on a local disk", name, fs)})
	case fs != "":
		d.add(&Check{Check: "state", Status: "ok", Name: fs, Message: fmt.Sprintf("%s is on a local filesystem (%s)", name, fs)})
	default:
		d.add(&Check{Check: "state", Status: "ok", Message: name + " is on a local filesystem"})
	}
}
