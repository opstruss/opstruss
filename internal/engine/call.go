package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"slices"

	"github.com/opstruss/opstruss/internal/builtin"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// callByName runs a call of a task by name, in a task or a step, as often
// as retry allows, with its inputs evaluated at each attempt.
func (e *engine) callByName(ctx context.Context, p progSpec, name string, args map[string]any, s *expr.Scope) *taskResult {
	t, err := e.p.Resolve(name)
	if err != nil {
		e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: err.Error()})
		return failed(err)
	}
	if t.Package != nil && t.Package.Decl != nil {
		// The env and pass of a declaration go to what it declares, under
		// those of the call.
		env := maps.Clone(t.Package.Decl.Env)
		if env == nil {
			env = model.Env{}
		}
		maps.Copy(env, p.env)
		p.env, p.pass = env, append(append([]string{}, t.Package.Decl.Pass...), p.pass...)
	}
	// kv writes when value is written, also as null, and checks expect
	// when it is written, also as null; their values do not say so.
	given := func(k string) bool {
		_, ok := args[k]
		if !ok && t.Package != nil && t.Package.Decl != nil {
			_, ok = t.Package.Decl.With[k]
		}
		return ok
	}
	return e.try(ctx, p, s, func(s *expr.Scope) (*taskResult, map[string]any, error) {
		in, marks, err := e.callInputs(t, args, s)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if t.Kind == model.TargetBuiltin && t.Task == "command" {
			return e.commandCall(ctx, p, in, marks, s)
		}
		dir, err := expr.EvalText(p.dir, s)
		if err != nil {
			return nil, nil, fmt.Errorf("dir: %w", err)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(p.base, dir)
		}
		if t.Kind == model.TargetTask {
			r := e.callTask(ctx, name, t.Package.Plugin.Tasks[t.Task], dir, in, call{unit: p.unit, plugin: pluginRunOf(t, p), unitRun: p.unitRun, marks: marks})
			return r, r.outputs, nil
		}
		var res *builtin.Result
		var outMarks map[string]any
		if t.Task == "kv" {
			res, outMarks = e.kvCall(ctx, in, marks, given("value"), given("expect"))
		} else {
			res = builtin.Run(ctx, &builtin.Call{Task: t.Task, Inputs: in, Dir: dir, Sensitive: marks["content"] != nil,
				Log: func(level, msg string) {
					e.emit(Event{Type: "log", Level: level, Task: p.label, Step: p.step, Message: msg})
				}})
		}
		if outMarks == nil {
			outMarks = map[string]any{}
		}
		if t.Task == "set" {
			// set gives its inputs as its outputs.
			maps.Copy(outMarks, marks)
		}
		r := &taskResult{outcome: success, changed: res.Changed, outputs: res.Outputs,
			marks: e.markContent(res.Outputs, outMarks, Event{Task: p.label, Step: p.step})}
		if res.Err != nil {
			r.outcome = failure
			if p.failedWhen == nil {
				e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: fmt.Sprintf("%s: %v", name, res.Err)})
			}
		}
		return r, res.Outputs, nil
	})
}

// pluginRun is what the programs of a plugin task run with: the plugin's
// directory, which relative programs in argv start from, the variables it
// asks for in env: that the project lets through with pass:, and the env
// of the call, over the plugin's own.
type pluginRun struct {
	name     string // in uses:
	dir      string
	pass     []string
	env      model.Env
	requires []string
	inputs   map[string]*model.Input // of the plugin and the task
}

// unitRun is what the programs of a task that runs for a unit get from it:
// its env and the variables it lets through, evaluated already; its state
// key and why it runs; and, under its lock, its work directory, or its
// record to read as $OPSTRUSS_STATE and tool/ of state: tool.
type unitRun struct {
	env      map[string]model.EnvValue
	pass     []string
	stateKey string
	reason   string
	workdir  string
	state    any
	stateDir string
}

func pluginRunOf(t *model.Target, p progSpec) *pluginRun {
	pl := t.Package.Plugin
	inputs, _ := t.Inputs()
	run := &pluginRun{name: t.Package.Name, dir: t.Package.Dir, env: p.env, requires: pl.Requires, inputs: inputs}
	for _, n := range pl.Env {
		for _, pat := range p.pass {
			if ok, _ := path.Match(pat, n); ok {
				run.pass = append(run.pass, n)
				break
			}
		}
	}
	return run
}

// pluginScope is what the expressions of a plugin task read: its inputs,
// its steps, and the facts, with opstruss.dir the plugin's directory.
func (e *engine) pluginScope(dir string, inputs, marks map[string]any, unit *instance) *expr.Scope {
	facts := e.facts(unit)
	facts["dir"], facts["artifacts"], facts["temp"], facts["cwd"] = dir, e.run.Artifacts, e.run.Temp, e.opts.Cwd
	s := &expr.Scope{Dir: dir, Vars: map[string]any{"inputs": inputs, "opstruss": facts, "steps": map[string]any{}}}
	if len(marks) > 0 {
		s.Marks = map[string]any{"inputs": marks}
	}
	k := e.newKV()
	return s.WithKV(k.get, k.marked)
}

// checkRequires fails a task whose programs are not in PATH: those of its
// requires:, and for a task of a plugin those of the plugin.
func (e *engine) checkRequires(t *model.Task, c call, s *expr.Scope) error {
	names, err := requires(t.Requires, s)
	if err != nil {
		return fmt.Errorf("requires: %w", err)
	}
	if c.plugin != nil {
		names = append(slices.Clone(c.plugin.requires), names...)
	}
	for _, n := range names {
		if _, err := plugin.LookPath(n, e.path()); err != nil {
			if c.plugin != nil {
				return fmt.Errorf("plugin %s requires %s, which is not in PATH", c.plugin.name, n)
			}
			return fmt.Errorf("requires: %w", err)
		}
	}
	return nil
}

// callInputs gives a call its inputs: those of the declaration's with:
// that the task takes, then the call's own, evaluated and checked against
// the task's schema; with the marks of those that are sensitive, because
// they read a sensitive value or the task declares them so.
func (e *engine) callInputs(t *model.Target, args map[string]any, s *expr.Scope) (map[string]any, map[string]any, error) {
	decl, typed := t.Inputs()
	given := map[string]any{}
	if t.Package != nil && t.Package.Decl != nil {
		for k, v := range t.Package.Decl.With {
			if !typed || decl[k] != nil {
				given[k] = v
			}
		}
	}
	maps.Copy(given, args)
	out, marks := map[string]any{}, map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(given)) {
		val, m, err := expr.EvalMarked(given[k], s)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = val
		switch {
		case decl[k] != nil && decl[k].Sensitive:
			e.known("input "+k, val)
			marks[k] = true
		case m != nil:
			e.hide(val, m)
			marks[k] = m
		}
	}
	if !typed {
		return out, marks, nil
	}
	out, err := conform(decl, out)
	return out, marks, err
}

// commandCall runs a call of command through the program contract, as run
// does, with argv or script, shell, and stdin from its inputs.
func (e *engine) commandCall(ctx context.Context, p progSpec, in, marks map[string]any, s *expr.Scope) (*taskResult, map[string]any, error) {
	if (in["argv"] == nil) == (in["script"] == nil) {
		return nil, nil, errors.New("command takes exactly one of argv or script")
	}
	if marks["argv"] != nil || e.redact.Contains(in["argv"]) {
		return nil, nil, errArgv
	}
	if sh, ok := in["shell"].(string); ok {
		p.shell = sh
	}
	c, err := e.programCall(p, s)
	if err != nil {
		return nil, nil, err
	}
	if script, ok := in["script"].(string); ok {
		c.Command = model.Command{Script: script}
	} else {
		for _, a := range in["argv"].([]any) {
			c.Command.Argv = append(c.Command.Argv, a.(string))
		}
	}
	if st, ok := in["stdin"].(map[string]any); ok {
		format, _ := st["format"].(string)
		if c.Stdin, err = plugin.Encode(st["value"], format); err != nil {
			return nil, nil, fmt.Errorf("stdin: %w", err)
		}
	}
	p.call = true
	return e.execProgram(ctx, p, c)
}
