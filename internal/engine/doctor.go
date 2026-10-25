package engine

import (
	"cmp"
	"errors"
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
	"github.com/opstruss/opstruss/internal/sources"
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
// programs of requires:, the data key and sops, the variables that
// plugins ask for, and where .opstruss is.
func Doctor(opts Options) (*Report, error) {
	e, err := e0(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	d := &doctor{e: e, r: &Report{Stack: e.p.StackName, Checks: []*Check{}}, need: map[string][]string{},
		passes: map[*model.Package][]string{}}
	d.units()
	d.tasks()
	d.programs()
	d.key()
	d.environment()
	d.pluginPaths()
	d.state()
	return d.r, nil
}

type doctor struct {
	search string

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
		search := d.search
		if search == "" {
			search = d.e.path()
		}
		key := n + "\x00" + search
		if !slices.Contains(d.need[key], what) {
			d.need[key] = append(d.need[key], what)
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
		d.search = ""
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
			d.search = d.e.pluginPath(t.Package)
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
	d.search = ""
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
	d.search = ""
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
	previous := d.search
	defer func() { d.search = previous }()
	d.search = d.e.pluginPath(t.Package)
	d.note(what, t.Package.Plugin.Requires...)
	d.passes[t.Package] = append(d.passes[t.Package], passes...)
	d.requires(what, t.Package.Plugin.Tasks[t.Task].Requires, &expr.Scope{Dir: t.Package.Dir,
		Vars: map[string]any{"stack": d.e.p.StackName, "opstruss": d.e.facts(nil)}}, true)
}

// programs checks that each program that requires: names is in PATH.
func (d *doctor) programs() {
	for _, key := range slices.Sorted(maps.Keys(d.need)) {
		name, search, _ := strings.Cut(key, "\x00")
		if search == "" {
			search = d.e.path()
		}
		what := d.need[key]
		slices.Sort(what)
		c := &Check{Check: "program", Name: name, For: what}
		if p, err := plugin.LookPath(name, search); err == nil {
			c.Status, c.Path, c.Message = "ok", p, fmt.Sprintf("%s at %s, for %s", name, p, strings.Join(what, ", "))
		} else {
			c.Status, c.Message = "error", fmt.Sprintf("%s is not in PATH, for %s", name, strings.Join(what, ", "))
		}
		d.add(c)
	}
	d.r.Checks = append(d.r.Checks, d.skip...)
}

// key checks the data key of the stack, when the stack needs one: it has
// one already, or the project has secrets or sensitive values. It reports
// the age key of this machine, which key opens the data key, or whether a
// run here can make it, and how encryption_keys differs from what wraps
// it. Sops is checked only for the secrets of sops files and the keys of
// encryption_keys that sops wraps for.
func (d *doctor) key() {
	e := d.e
	meta := state.MetaKey(e.p.StackName)
	m, _, err := e.readMeta()
	if err != nil {
		d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf("reading %s: %v", meta, err)})
		return
	}
	hasKey := m != nil && m.Key != nil
	needKey := hasKey || e.p.HasSensitive()
	fromFiles := slices.ContainsFunc(slices.Collect(maps.Values(e.p.Secrets)), func(s *model.Secret) bool { return s.Sops != "" })
	forSops := slices.ContainsFunc(e.p.EncryptionKeys, func(k *model.EncryptionKey) bool { return k.Sops != nil })
	if fromFiles || needKey && forSops {
		d.sops()
	}
	if !needKey {
		return
	}
	ids, err := secret.Identities(e.opts.LookupEnv)
	if err != nil {
		d.add(&Check{Check: "key", Status: "error", Message: fmt.Sprintf("the age keys of this machine: %v", err)})
		return
	}
	mine := "the age key of this machine"
	if len(ids) > 0 {
		mine = fmt.Sprintf("the age key of this machine, %s,", ids[0].Recipient())
		d.add(keyCheck(ids[0], fmt.Sprintf("the age key of this machine is %s, from %s", ids[0].Recipient(), ids[0].From)))
	} else if file, err := secret.KeyFile(e.opts.LookupEnv); err == nil {
		d.add(&Check{Check: "key", Status: "ok", Path: file, Message: fmt.Sprintf(
			"this machine has no age key yet; the first run that needs one makes it in %s", file)})
	} else {
		d.add(&Check{Check: "key", Status: "error", Message: fmt.Sprintf("this machine has no age key, and no file to make one in: %v", err)})
	}
	switch {
	case hasKey:
		_, route, err := e.openEnvelopes(m.Key)
		var closed *closedError
		switch {
		case errors.As(err, &closed):
			d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf(
				"the data key of stack %s does not open on this machine: %v; add %s to encryption_keys, and run plan, up, or down on a machine that opens it",
				e.p.StackName, closed, mine)})
		case err != nil:
			d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf("the data key of stack %s does not open: %v", e.p.StackName, err)})
		default:
			d.add(&Check{Check: "key", Status: "ok", Name: meta, Message: fmt.Sprintf("the data key of stack %s opens %s", e.p.StackName, route)})
		}
		if diff := e.keysDiff(m.Key); diff != "" {
			d.add(&Check{Check: "key", Status: "warn", Name: meta, Message: fmt.Sprintf(
				"encryption_keys differs from what wraps the data key in %s (%s); the next plan, up, down, or task run here wraps it again", meta, diff)})
		}
	case e.p.EncryptionKeys == nil:
		d.add(&Check{Check: "key", Status: "ok", Name: meta, Message: fmt.Sprintf(
			"stack %s has no data key yet; the first value that needs one makes it for %s", e.p.StackName, strings.TrimSuffix(mine, ","))})
	case slices.ContainsFunc(e.p.EncryptionKeys, func(k *model.EncryptionKey) bool {
		return slices.ContainsFunc(ids, func(id secret.Identity) bool { return id.Recipient() == k.Age })
	}):
		d.add(&Check{Check: "key", Status: "ok", Name: meta, Message: fmt.Sprintf(
			"stack %s has no data key yet; the first value that needs one makes it for encryption_keys", e.p.StackName)})
	case forSops:
		d.add(&Check{Check: "key", Status: "skip", Name: meta, Message: fmt.Sprintf(
			"stack %s has no data key yet, and none of the age recipients of encryption_keys is a key of this machine; only a run can tell whether sops opens what it makes", e.p.StackName)})
	default:
		d.add(&Check{Check: "key", Status: "error", Name: meta, Message: fmt.Sprintf(
			"encryption_keys has no key of this machine, so a run here cannot make the data key of stack %s; add %s to encryption_keys", e.p.StackName, mine)})
	}
}

// keyCheck reports where the age key of this machine came from: a
// variable or a file.
func keyCheck(id secret.Identity, msg string) *Check {
	c := &Check{Check: "key", Status: "ok", Message: msg}
	if strings.HasPrefix(id.From, "$") {
		c.Name = strings.TrimPrefix(id.From, "$")
	} else {
		c.Path = id.From
	}
	return c
}

// sops checks that sops is in PATH and new enough.
func (d *doctor) sops() bool {
	p, err := plugin.LookPath("sops", d.e.path())
	if err != nil {
		d.add(&Check{Check: "sops", Status: "error", Name: "sops", Message: fmt.Sprintf(
			"sops is not in PATH, and the stack needs it for the secrets of sops files or the keys of encryption_keys that are not age recipients; install %s or newer, for example with mise use sops", secret.MinSops)})
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

// environment warns about the variables that a plugin asks for, that the
// operator set to a value, and that no pass: of what uses it lets
// through, so its programs never get them; one that is not set misses
// nothing, and an official plugin asks for many that are optional, such
// as TF_LOG.
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
			if !slices.ContainsFunc(d.e.opts.Environ, func(kv string) bool {
				name, value, _ := strings.Cut(kv, "=")
				ok, _ := path.Match(v, name)
				return ok && value != ""
			}) {
				continue
			}
			d.add(&Check{Check: "env", Status: "warn", Name: v, For: []string{"plugin " + pkg.Name}, Message: fmt.Sprintf(
				"plugin %s asks for %s, and no pass: of what uses it lets it through, so its programs never get it", pkg.Name, v)})
		}
	}
}

// pluginPaths warns about a program that a local plugin outside a catalog
// names by a path outside its directory: the hash of the plugin leaves
// that file out, so a change there runs none of its units again.
func (d *doctor) pluginPaths() {
	pkgs := slices.SortedFunc(maps.Keys(d.passes), func(a, b *model.Package) int {
		return cmp.Or(strings.Compare(a.Dir, b.Dir), strings.Compare(a.Name, b.Name))
	})
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		if pkg.Source != nil || pkg.Catalog != nil || seen[pkg.Dir] {
			continue
		}
		seen[pkg.Dir] = true
		for _, prog := range pkg.Plugin.RelativePrograms() {
			// A symlink in the directory counts with its target; a
			// directory that leads elsewhere does not.
			if ok, err := sources.Inside(pkg.Dir, []string{"."}, filepath.Dir(filepath.Join(pkg.Dir, prog))); ok || err != nil {
				continue
			}
			d.add(&Check{Check: "program", Status: "warn", Name: prog, For: []string{"plugin " + pkg.Name}, Message: fmt.Sprintf(
				"plugin %s runs %s, outside its directory, so a change there runs none of its units again; mark the root that holds both with %s and name the path in its shared:",
				pkg.Name, prog, model.CatalogFile)})
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
