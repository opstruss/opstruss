package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/builtin"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// taskUnit is what a unit that uses a task runs at up: a task of the
// project, of a plugin, or a built-in task, with the unit's with: as its
// inputs.
type taskUnit struct {
	target *model.Target
	inputs map[string]any
	hash   string // of the plugin's directory, for a task of a plugin
}

// resolveTask fills r for a unit that uses a task: its inputs, and the
// env and variables of the declaration under the unit's; command runs as
// a run unit does.
func (e *engine) resolveTask(inst *instance, r *resolved, s *expr.Scope) error {
	u, t := inst.def, inst.def.Target
	in, marks, err := e.callInputs(t, u.With, s)
	if err != nil {
		return fmt.Errorf("with: %w", err)
	}
	for k, v := range in {
		if marks[k] != nil || e.redact.Contains(v) {
			r.inputHidden[k] = true
		}
	}
	tu := &taskUnit{target: t, inputs: in}
	allowed := slices.Clone(u.Pass)
	if d := t.Package; d != nil && d.Decl != nil {
		env := map[string]model.EnvValue{}
		for k, v := range d.Decl.Env {
			text, sensitive, err := expr.EvalTextMarked(v.Text, s)
			if err != nil {
				return fmt.Errorf("env.%s: %w", k, err)
			}
			env[k] = model.EnvValue{Text: text, File: v.File}
			if _, set := r.env[k]; !set {
				r.envHidden[k] = sensitive
			}
			if sensitive {
				e.hide(text, true)
			}
		}
		maps.Copy(env, r.env)
		r.env = env
		allowed = append(allowed, d.Decl.Pass...)
	}
	r.pass = map[string]string{}
	for _, kv := range e.opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		ok := slices.ContainsFunc(allowed, func(p string) bool { m, _ := path.Match(p, k); return m }) && !model.ReservedEnv(k)
		// A plugin gets only the variables it asks for.
		if t.Kind == model.TargetTask && !slices.Contains(t.Package.Plugin.Env, k) {
			ok = false
		}
		if ok && inst.node.Grants(k) {
			r.pass[k] = v
		}
	}
	switch {
	case t.Kind == model.TargetTask:
		if tu.hash, err = e.pluginHash(t.Package); err != nil {
			return err
		}
		r.requires = append(slices.Clone(t.Package.Plugin.Requires), r.requires...)
	case t.Kind == model.TargetBuiltin && t.Task == "command":
		if (in["argv"] == nil) == (in["script"] == nil) {
			return errors.New("with: command takes exactly one of argv or script")
		}
		if script, ok := in["script"].(string); ok {
			r.cmd = model.Command{Script: script}
		} else {
			for _, a := range in["argv"].([]any) {
				r.cmd.Argv = append(r.cmd.Argv, a.(string))
			}
		}
		if sh, ok := in["shell"].(string); ok {
			r.shell = sh
		}
		if st, ok := in["stdin"].(map[string]any); ok {
			format, _ := st["format"].(string)
			if r.stdin, err = plugin.Encode(st["value"], format); err != nil {
				return fmt.Errorf("with: stdin: %w", err)
			}
		}
	}
	r.task = tu
	return nil
}

// taskSource is what a unit that uses the task name of the project runs:
// the task as written, with the tasks it calls through task: and needs,
// their remote plugin sources, and programs named by a relative path.
func (e *engine) taskSource(in *model.Project, name string, out map[string]any) error {
	if _, seen := out[name]; seen || in.Tasks[name] == nil {
		return nil
	}
	tasks, _ := in.Doc()["tasks"].(map[string]any)
	out[name] = tasks[name]
	t, dir := in.Tasks[name], in.DirOf("tasks."+name)
	for _, callee := range model.CallNames(t) {
		target, err := in.Resolve(callee)
		if err != nil {
			return err
		}
		if pkg := target.Package; pkg != nil && pkg.Source != nil {
			out["source "+callee] = pkg.Source
		}
	}
	runs := []*model.Command{t.Run}
	var called []string
	var walk func([]*model.Step)
	walk = func(steps []*model.Step) {
		for _, s := range steps {
			runs = append(runs, s.Run)
			if in.Tasks[s.Task] != nil {
				called = append(called, s.Task)
			}
			walk(s.Block)
			walk(s.Rescue)
			walk(s.Always)
		}
	}
	walk(t.Steps)
	for _, c := range runs {
		if c == nil || len(c.Argv) == 0 || !strings.Contains(c.Argv[0], "/") || strings.Contains(c.Argv[0], "${{") {
			continue
		}
		prog := c.Argv[0]
		if !filepath.IsAbs(prog) {
			prog = filepath.Join(dir, prog)
		}
		sum, err := hashFile(prog)
		if err != nil {
			return fmt.Errorf("task %s: program: %w", name, err)
		}
		out["program "+name+" "+c.Argv[0]] = sum
	}
	for _, n := range append(called, t.Needs...) {
		if err := e.taskSource(in, n, out); err != nil {
			return err
		}
	}
	return nil
}

// execTask runs the task of a unit that uses one as the program of its
// up: under its lock, with what the unit gives its programs, and with the
// task's outputs as the unit's.
func (e *engine) execTask(ctx context.Context, inst *instance, r *resolved, rec *state.Record, reason string) *plugin.Result {
	t := r.task.target
	ur := &unitRun{env: r.env, pass: slices.Sorted(maps.Keys(r.pass)), stateKey: rec.StateKey, reason: reason,
		workdir: e.workdir(inst.address), state: programState(rec)}
	// The unit's approval covers what its task asks.
	c := call{frame: inst.frame, unit: inst, yes: true, source: "unit", unitRun: ur}
	var res *taskResult
	switch t.Kind {
	case model.TargetProject:
		in := inst.node.In
		res = e.callTask(ctx, t.Task, in.Tasks[t.Task], in.DirOf("tasks."+t.Task), r.task.inputs, c)
		if in == e.p {
			e.ranTask(t.Task, res.outcome)
		}
	case model.TargetTask:
		inputs, _ := t.Inputs()
		c.plugin = &pluginRun{name: t.Package.Name, dir: t.Package.Dir, requires: t.Package.Plugin.Requires, inputs: inputs}
		res = e.callTask(ctx, t.Name, t.Package.Plugin.Tasks[t.Task], r.cwd, r.task.inputs, c)
	default:
		var out *builtin.Result
		if t.Task == "kv" {
			given := func(k string) bool {
				_, ok := inst.def.With[k]
				if !ok && t.Package != nil && t.Package.Decl != nil {
					_, ok = t.Package.Decl.With[k]
				}
				return ok
			}
			out, _ = e.kvCall(ctx, r.task.inputs, nil, given("value"), given("expect"))
		} else {
			out = builtin.Run(ctx, &builtin.Call{Task: t.Task, Inputs: r.task.inputs, Dir: r.cwd, Log: func(level, msg string) {
				e.emit(Event{Type: "log", Level: level, Address: inst.address, Task: "up", Message: msg})
			}})
		}
		res = &taskResult{outcome: success, changed: out.Changed, outputs: out.Outputs}
		if out.Err != nil {
			res.outcome = failure
			e.emit(Event{Type: "step", Level: "error", Address: inst.address, Task: "up", Message: fmt.Sprintf("%s: %v", t.Name, out.Err)})
		}
	}
	out := &plugin.Result{Outputs: res.outputs, Changed: res.changed, Sensitive: slices.Sorted(maps.Keys(res.marks))}
	if out.Outputs == nil {
		out.Outputs = map[string]any{}
	}
	if res.outcome != success {
		out.OutputErr = fmt.Errorf("task %s ended with %s", t.Name, res.outcome)
	}
	return out
}
