package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// pluginUnit is what a unit that uses a plugin runs: the plugin's plan,
// up, and down with the unit's inputs.
type pluginUnit struct {
	target *model.Target
	dir    string         // the plugin's directory
	inputs map[string]any // the plugin's inputs, from the declaration's with: and the unit's
	plan   *model.Command // nil when the plugin has no plan
	hash   string         // of what the plugin's directory holds, as h1: is
	tool   string         // tool/, where a plugin with state: tool keeps the tool's state
	env    model.Env      // the env of the declaration and the unit, evaluated
	base   string         // the unit's working directory, relative to its file, before dir: of up
}

// unitOf is what the programs of a unit that uses a plugin receive.
func (e *engine) unitOf(inst *instance, r *resolved, rec *state.Record) *plugin.Unit {
	u := &plugin.Unit{Address: inst.address, Workdir: e.workdir(inst.address)}
	if rec != nil {
		u.StateKey, u.State = rec.StateKey, programState(rec)
	}
	if r.plugin != nil {
		u.StateDir = r.plugin.tool
	}
	return u
}

// resolvePlugin fills r for a unit that uses a plugin: the inputs from
// with:, the plugin's programs with them, its env under the unit's, and
// the variables the plugin asks for that the unit or the declaration lets
// through.
func (e *engine) resolvePlugin(inst *instance, r *resolved, s *expr.Scope) error {
	u, t := inst.def, inst.def.Target
	pl := t.Package.Plugin
	given := map[string]any{}
	if d := t.Package.Decl; d != nil {
		maps.Copy(given, d.With)
	}
	maps.Copy(given, u.With)
	v, withMarks, err := expr.EvalMarked(given, s)
	if err != nil {
		return fmt.Errorf("with: %w", err)
	}
	marks, _ := withMarks.(map[string]any)
	inputMarks := map[string]any{}
	for k, val := range v.(map[string]any) {
		// A sensitive value is known before the schema checks it, so the
		// error of the check hides it.
		switch {
		case pl.Inputs[k] != nil && pl.Inputs[k].Sensitive:
			e.known("input "+k+" of "+e.short(inst.address), val)
		case marks[k] != nil:
			e.hide(val, marks[k])
		case !e.redact.Contains(val):
			continue
		}
		r.inputHidden[k], inputMarks[k] = true, true
	}
	inputs, err := conform(pl.Inputs, v.(map[string]any))
	if err != nil {
		return fmt.Errorf("with: %w", err)
	}
	hash, err := e.pluginHash(t.Package)
	if err != nil {
		return err
	}
	pu := &pluginUnit{target: t, dir: t.Package.Dir, inputs: inputs, hash: hash}
	if pl.State == "tool" {
		if dir, ok := e.store.ToolDir(inst.address); ok {
			pu.tool = dir
		}
	}
	ps := e.pluginScope(pu.dir, inputs, inputMarks, inst)
	up := pl.Tasks["up"]
	if r.cmd, err = e.command(up.Run, ps); err != nil {
		return fmt.Errorf("up: %w", err)
	}
	r.shell = up.Shell
	if d := pl.Tasks["down"]; d != nil {
		cmd, err := e.command(d.Run, ps)
		if err != nil {
			return fmt.Errorf("down: %w", err)
		}
		r.down = &cmd
	}
	if p := pl.Tasks["plan"]; p != nil {
		cmd, err := e.command(p.Run, ps)
		if err != nil {
			return fmt.Errorf("plan: %w", err)
		}
		pu.plan = &cmd
	}
	pu.base = r.dir
	if up.Dir != "" {
		d, err := expr.EvalText(up.Dir, ps)
		if err != nil {
			return fmt.Errorf("up: dir: %w", err)
		}
		r.dir, r.cwd = filepath.Join(r.dir, d), filepath.Join(r.cwd, d)
	}
	// The plugin's env, under the declaration's and the unit's.
	var allowed []string
	own := model.Env{}
	if d := t.Package.Decl; d != nil {
		for k, v := range d.Env {
			text, sensitive, err := expr.EvalTextMarked(v.Text, s)
			if err != nil {
				return fmt.Errorf("env.%s: %w", k, err)
			}
			own[k] = model.EnvValue{Text: text, File: v.File}
			if _, set := r.env[k]; !set {
				r.envHidden[k] = sensitive
			}
			if sensitive {
				e.hide(text, true)
			}
		}
		allowed = append(allowed, d.Pass...)
	}
	maps.Copy(own, r.env)
	env := map[string]model.EnvValue{}
	for k, v := range up.Env {
		text, sensitive, err := expr.EvalTextMarked(v.Text, ps)
		if err != nil {
			return fmt.Errorf("up: env.%s: %w", k, err)
		}
		env[k] = model.EnvValue{Text: text, File: v.File}
		if _, set := own[k]; !set {
			r.envHidden[k] = sensitive
		}
		if sensitive {
			e.hide(text, true)
		}
	}
	maps.Copy(env, own)
	r.env, pu.env = env, own
	allowed = append(allowed, u.Pass...)
	r.pass = map[string]string{}
	for _, kv := range e.opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		if inst.node.Grants(k) && slices.Contains(pl.Env, k) && slices.ContainsFunc(allowed, func(p string) bool { ok, _ := path.Match(p, k); return ok }) {
			r.pass[k] = v
		}
	}
	reqs, err := requires(up.Requires, ps)
	if err != nil {
		return fmt.Errorf("requires: %w", err)
	}
	r.requires = append(append(slices.Clone(pl.Requires), reqs...), r.requires...)
	if u.Stop == nil && up.Stop != nil {
		if r.stop, err = plugin.StopOf(up.Stop); err != nil {
			return err
		}
	}
	r.plugin = pu
	return nil
}

// pluginHash is the hash of the package of a plugin, as the h1: hash of
// Go modules is: a script that changes changes the definition of the units
// that use it.
func (e *engine) pluginHash(pkg *model.Package) (string, error) {
	return e.packageHash(pkg.Source, pkg.Catalog, pkg.Dir)
}

// packageHash is the hash of a package by its content, read once a run: in
// a catalog its directory and the shared paths, for another remote source
// the whole tree of its commit, and for a local plugin its directory.
func (e *engine) packageHash(id *sources.Identity, cat *model.Catalog, dir string) (string, error) {
	if id != nil && cat == nil {
		return id.Hash, nil
	}
	if cat != nil {
		dir = filepath.Join(cat.Root, filepath.FromSlash(cat.Path))
	}
	e.mu.Lock()
	h, ok := e.hashes[dir]
	e.mu.Unlock()
	if ok {
		return h, nil
	}
	var err error
	switch {
	case id != nil:
		h, err = e.p.Sources.Cache.PackageHash(*id, cat.Path, cat.Shared)
	case cat != nil:
		h, err = sources.LocalPackageHash(cat.Root, cat.Path, cat.Shared)
	default:
		h, err = sources.LocalHash(dir)
	}
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.hashes[dir] = h
	e.mu.Unlock()
	return h, nil
}

// StatusUnits runs status of the units: the plugin's status task for a
// unit whose plugin has one, without the unit's lock and with its record
// to read, what its supervisor says of a service, and otherwise what the
// record says: up, pending, or absent, with the last run and the outputs.
func StatusUnits(ctx context.Context, opts Options) (*Result, error) {
	e, err := open(ctx, opts, "status", "")
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	ctx = e.ctx
	got, _, err := e.statusPass(ctx, opts.Units)
	if err != nil {
		e.fail("", err)
	}
	for _, u := range got {
		if u.(map[string]any)["outcome"] == failure {
			e.exit = max(e.exit, 1)
		}
	}
	outcome := map[int]string{0: success, 1: failure, 2: skipped}[e.exit]
	e.close(outcome, nil, nil)
	return e.result(), nil
}

// statusPass runs status of the units that patterns pick, and returns, for
// each by its address without the stack, the outcome and outputs, which a
// task: status step gives as steps.<id>.units, with the marks of the
// sensitive ones.
func (e *engine) statusPass(ctx context.Context, patterns []string) (map[string]any, map[string]any, error) {
	e.passMu.Lock()
	e.pass = pass{units: patterns, noDeps: true}
	e.resetUnits()
	err := e.selectUnits(false)
	if err == nil {
		e.previewQuiet()
	}
	insts := slices.Clone(e.insts)
	e.passMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if len(insts) == 0 {
		e.say("run", "", "no units")
	}
	out, marks := map[string]any{}, map[string]any{}
	for _, inst := range insts {
		if !e.sel.has(inst) {
			continue
		}
		short := e.short(inst.address)
		if t := inst.def.Target; t != nil && t.Kind == model.TargetPlugin && t.Package.Plugin.Tasks["status"] != nil {
			r, err := e.resolve(inst, false)
			if err != nil {
				e.fail(inst.address, err)
				continue
			}
			run := &pluginRun{name: t.Package.Name, dir: r.plugin.dir, requires: t.Package.Plugin.Requires, inputs: taskInputs(t.Package.Plugin, "status")}
			ur := &unitRun{env: r.plugin.env, pass: slices.Sorted(maps.Keys(r.pass)), stateDir: r.plugin.tool}
			if inst.record != nil {
				ur.state = programState(inst.record)
			}
			res := e.callTask(ctx, "status", t.Package.Plugin.Tasks["status"], inst.dir, r.plugin.inputs,
				call{unit: inst, plugin: run, unitRun: ur})
			out[short] = res.view()
			if m := res.viewMarks(); m != nil {
				marks[short] = m
			}
			continue
		}
		outputs := map[string]any{"state": "absent", "run": "", "updated": "", "outputs": map[string]any{}}
		ev := Event{Type: "unit", Level: "info", Address: inst.address}
		switch rec := inst.record; {
		case inst.def.Service != nil:
			outputs = e.serviceStatus(inst)
			ev.Message = fmt.Sprint(outputs["state"])
			if pid := outputs["pid"].(int64); pid != 0 {
				ev.Message += fmt.Sprintf(", pid %d since %s, %d restarts", pid, outputs["since"], outputs["restarts"])
			}
		case rec == nil || rec.Tombstone != nil:
			ev.Message = "absent"
		default:
			outputs["state"], outputs["run"], outputs["updated"] = "up", rec.Run, rec.Updated
			if shown := e.shown(rec.Outputs, hiddenMarks(rec.Hidden[state.PartOutputs])); shown != nil {
				outputs["outputs"] = shown
			}
			ev.Message = fmt.Sprintf("up, from run %s at %s", rec.Run, rec.Updated)
			if rec.Pending != nil {
				outputs["state"] = "pending"
				ev.Message = "pending: " + pendingReason(rec.Pending)
			}
		}
		ev.Outputs = outputs
		e.emit(ev)
		if o, _ := outputs["outputs"].(map[string]any); len(o) > 0 && outputs["state"] == "up" {
			e.say("unit", inst.address, "outputs: %s", expr.JSON(o))
		}
		out[short] = map[string]any{"outcome": success, "changed": false, "outputs": outputs}
	}
	return out, marks, nil
}

// toolMoved stops a unit whose tool keeps state in tool/ while its input
// backend names another place: that backend is empty, as terraform init
// refuses a backend that changed without -migrate-state.
func (e *engine) toolMoved(inst *instance, r *resolved) error {
	if r.plugin == nil || r.plugin.tool == "" {
		return nil
	}
	if b := r.plugin.inputs["backend"]; b == nil || b == "" {
		return nil
	}
	if ents, err := os.ReadDir(r.plugin.tool); err != nil || len(ents) == 0 {
		return nil
	}
	short := e.short(inst.address)
	return fmt.Errorf("the tool's state is still in tool/ while input backend names another place; move it with opstruss exec %s -- <the tool's command that moves state>, then opstruss state rm %s --tool", short, short)
}

// pluginPlan is what a plugin's plan wrote to $OPSTRUSS_PLAN.
type pluginPlan struct {
	Changes []map[string]any `json:"changes"`
	Outputs []string         `json:"outputs"`
	Digest  string           `json:"digest"`
	Exact   bool             `json:"exact"`
}

// planPlugin runs the plan of a plugin for inst under its lock, with the
// work directory that up of the same turn finds again.
func (e *engine) planPlugin(ctx context.Context, inst *instance, r *resolved, rec *state.Record, held *heldLogs) (*pluginPlan, error) {
	u := e.unitOf(inst, r, rec)
	out := &lineWriter{e: e, addr: inst.address, task: "plan", stream: "stdout", held: held}
	errw := &lineWriter{e: e, addr: inst.address, task: "plan", stream: "stderr", held: held}
	defer out.flush()
	defer errw.flush()
	pl := r.plugin.target.Package.Plugin
	res, err := e.run.Exec(ctx, &plugin.Call{Command: *r.plugin.plan, Shell: pl.Tasks["plan"].Shell, Base: inst.dir, Dir: r.dir,
		Env: r.env, Pass: slices.Sorted(maps.Keys(r.pass)), Stop: r.stop, Task: "plan", Reason: "requested", Plan: true,
		Unit: u, Inputs: r.plugin.inputs, PluginDir: r.plugin.dir, Stdout: out, Stderr: errw})
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		e.warn(inst.address, "%s", w)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("plan exited with %d", res.ExitCode)
	}
	pp := &pluginPlan{}
	if len(strings.TrimSpace(string(res.Plan))) == 0 {
		// A plan that says nothing cannot say that nothing changes.
		pp.Changes = []map[string]any{{"action": "run"}}
		return pp, nil
	}
	if err := json.Unmarshal(res.Plan, pp); err != nil {
		return nil, fmt.Errorf("$OPSTRUSS_PLAN: %w", err)
	}
	for i, c := range pp.Changes {
		if a, _ := c["action"].(string); !slices.Contains([]string{"create", "update", "delete", "replace", "run"}, a) {
			return nil, fmt.Errorf("$OPSTRUSS_PLAN: change %d has action %q, not create, update, delete, replace, or run", i, a)
		}
	}
	return pp, nil
}

// describe writes the changes of a plan as a plan line does.
func describe(changes []map[string]any) string {
	var parts []string
	for _, c := range changes {
		part, _ := c["action"].(string)
		if res, _ := c["resource"].(string); res != "" {
			part += " " + res
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// planDigest joins the digest of a plugin's plan to the digest trigger,
// so a plan approved for one definition never matches another.
func planDigest(trigger, plugin string) string {
	if plugin == "" {
		return trigger
	}
	sum := sha256.Sum256([]byte(trigger + "\n" + plugin))
	return "sha256:v1:" + fmt.Sprintf("%x", sum)
}

// pluginOf is the plugin's directory relative to the project root, as a
// down contract keeps it.
func (e *engine) pluginOf(r *resolved) string {
	if r.plugin == nil {
		return ""
	}
	rel, err := filepath.Rel(e.p.Dir, r.plugin.dir)
	if err != nil {
		return r.plugin.dir
	}
	return filepath.ToSlash(rel)
}
