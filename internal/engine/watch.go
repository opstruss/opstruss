package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// Filter decides for each event of a watch whether it goes on, by the if:
// of the trigger, with the project read once.
type Filter struct {
	e    *engine
	cond any
}

// NewFilter reads the project of opts for the if: of the watch t.
func NewFilter(opts Options, t *Trigger) (*Filter, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	w := e.p.Tasks[t.Task].On.Watch[t.Index]
	if w.If == nil {
		return &Filter{}, nil
	}
	e.ctx, e.trigger = context.Background(), true
	e.pass = pass{}
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewQuiet()
	return &Filter{e: e, cond: w.If}, nil
}

// Match reports whether the event with fields goes on.
func (f *Filter) Match(fields map[string]any) (bool, error) {
	if f.cond == nil {
		return true, nil
	}
	f.e.event = eventOf(&TriggerEvent{Trigger: "watch", Time: time.Now().UTC(), Fields: fields}, time.Time{})
	ok, err := expr.EvalCondition(f.cond, f.e.triggerScope())
	return ok, f.e.redact.Err(err)
}

// WatchSource prepares the program of a watch that runs one: one call of
// a task, command or a task of a plugin, or the watch hook of the plugin
// of a unit, with its inputs evaluated now, as they are each time the
// source starts. It works in the directory of the task or the unit that
// watches, as a step does; dir keeps the files of its env: values.
func WatchSource(opts Options, t *Trigger, dir string) (*plugin.Program, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	e.ctx, e.trigger = context.Background(), true
	e.pass = pass{}
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewQuiet()
	run := plugin.Outside(e.p.Name, e.p.StackName, opts.Environ)
	w := e.p.Tasks[t.Task].On.Watch[t.Index]
	if w.Unit != "" {
		insts := e.byUnit[w.Unit]
		if len(insts) == 0 {
			return nil, fmt.Errorf("unit %s has no instance", w.Unit)
		}
		inst := insts[0]
		r, err := e.resolve(inst, false)
		if err != nil {
			return nil, err
		}
		if r.plugin == nil || r.plugin.target.Package.Plugin.Tasks["watch"] == nil {
			return nil, fmt.Errorf("unit %s is no service, and its plugin has no watch hook", w.Unit)
		}
		hook := r.plugin.target.Package.Plugin.Tasks["watch"]
		cmd, err := e.command(hook.Run, &expr.Scope{Dir: r.plugin.dir, Vars: map[string]any{"inputs": r.plugin.inputs, "opstruss": e.facts(inst)}})
		if err != nil {
			return nil, fmt.Errorf("watch hook: %w", err)
		}
		return run.Prepare(&plugin.Call{Command: cmd, Shell: hook.Shell, Base: inst.dir, Dir: r.dir, Env: r.plugin.env,
			Pass: slices.Sorted(maps.Keys(r.pass)), Inputs: r.plugin.inputs, PluginDir: r.plugin.dir,
			Unit: &plugin.Unit{Address: inst.address}}, dir)
	}
	if len(w.Calls) != 1 {
		return nil, errors.New("a watch has exactly one source")
	}
	name := slices.Collect(maps.Keys(w.Calls))[0]
	target, err := e.p.Resolve(name)
	if err != nil {
		return nil, err
	}
	in, _, err := e.callInputs(target, w.Calls[name], e.triggerScope())
	if err != nil {
		return nil, e.redact.Err(fmt.Errorf("%s: %w", name, err))
	}
	base := e.p.DirOf("tasks." + t.Task)
	switch {
	case target.Kind == model.TargetBuiltin && target.Task == "command":
		cmd := model.Command{}
		if script, ok := in["script"].(string); ok {
			cmd.Script = script
		} else {
			for _, a := range in["argv"].([]any) {
				cmd.Argv = append(cmd.Argv, fmt.Sprint(a))
			}
		}
		return run.Prepare(&plugin.Call{Command: cmd, Base: base}, dir)
	case target.Kind == model.TargetTask:
		task := target.Package.Plugin.Tasks[target.Task]
		if task.Run == nil {
			return nil, fmt.Errorf("%s runs no program, so it gives no lines", name)
		}
		cmd, err := e.command(task.Run, &expr.Scope{Dir: target.Package.Dir, Vars: map[string]any{"inputs": in, "opstruss": e.facts(nil)}})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var env model.Env
		var pass []string
		if d := target.Package.Decl; d != nil {
			env, pass = d.Env, d.Pass
		}
		prog := pluginRunOf(target, progSpec{env: env, pass: pass})
		values := map[string]model.EnvValue{}
		for k, v := range env {
			values[k] = v
		}
		return run.Prepare(&plugin.Call{Command: cmd, Shell: task.Shell, Base: base, Dir: task.Dir, Env: values,
			Pass: prog.pass, Inputs: in, PluginDir: target.Package.Dir}, dir)
	}
	return nil, fmt.Errorf("%s is no program that writes a line per event", name)
}
