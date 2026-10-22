package engine

import (
	"cmp"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"sync"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// Logs shows the output of the units: the log of a service, the logs task
// of a plugin that has one, or the output of the last run that touched a
// unit, as docker logs shows that of a container that ended. With
// opts.Follow, the logs of services and the plugins that follow go on
// until ctx ends.
func Logs(ctx context.Context, opts Options) (*Result, error) {
	e, err := open(ctx, opts, "logs", "")
	if err != nil {
		return nil, err
	}
	got, _, err := e.logsPass(ctx, opts.Units, opts.Follow)
	if err != nil {
		e.fail("", err)
	}
	for _, u := range got {
		if u.(map[string]any)["outcome"] == failure {
			e.exit = max(e.exit, 1)
		}
	}
	e.close(map[int]string{0: success, 1: failure}[e.exit], nil, nil)
	return e.result(), nil
}

// logsPass shows the output of the units that patterns pick, each in graph
// order, or with follow all at once, and returns, for each by its address
// without the stack, its outcome, as a task: logs step gives it.
func (e *engine) logsPass(ctx context.Context, patterns []string, follow bool) (map[string]any, map[string]any, error) {
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
	var mu sync.Mutex
	var wg sync.WaitGroup
	out, marks := map[string]any{}, map[string]any{}
	for _, inst := range insts {
		if !e.sel.has(inst) {
			continue
		}
		show := func() {
			view, m := e.unitLogs(ctx, inst, follow)
			mu.Lock()
			defer mu.Unlock()
			out[e.short(inst.address)] = view
			if m != nil {
				marks[e.short(inst.address)] = m
			}
		}
		if follow {
			wg.Go(show)
		} else {
			show()
		}
	}
	wg.Wait()
	return out, marks, nil
}

// unitLogs shows the output of one unit.
func (e *engine) unitLogs(ctx context.Context, inst *instance, follow bool) (any, any) {
	done := map[string]any{"outcome": success, "changed": false, "outputs": map[string]any{}}
	failed := func(err error) any {
		e.fail(inst.address, err)
		return map[string]any{"outcome": failure, "changed": false, "outputs": map[string]any{}}
	}
	if t := inst.def.Target; t != nil && t.Kind == model.TargetPlugin && t.Package.Plugin.Tasks["logs"] != nil {
		r, err := e.resolve(inst, false)
		if err != nil {
			return failed(err), nil
		}
		pl := t.Package.Plugin
		task := pl.Tasks["logs"]
		given := maps.Clone(r.plugin.inputs)
		if given == nil {
			given = map[string]any{}
		}
		if task.Inputs["follow"] != nil {
			given["follow"] = follow
		}
		run := &pluginRun{name: t.Package.Name, dir: r.plugin.dir, requires: pl.Requires, inputs: taskInputs(pl, "logs")}
		ur := &unitRun{env: r.plugin.env, pass: slices.Sorted(maps.Keys(r.pass)), stateDir: r.plugin.tool}
		if inst.record != nil {
			ur.state = programState(inst.record)
		}
		res := e.callTask(ctx, "logs", task, inst.dir, given, call{unit: inst, plugin: run, unitRun: ur})
		return res.view(), res.viewMarks()
	}
	if inst.def.Service != nil {
		path := filepath.Join(e.root, "logs", e.p.StackName, filepath.FromSlash(e.short(inst.address))+".jsonl")
		if err := record.ReadLog(ctx, path, follow, e.replay); err != nil {
			return failed(err), nil
		}
		return done, nil
	}
	runs, err := runRecords(e.root, e.p.StackName)
	if err != nil {
		return failed(err), nil
	}
	for _, r := range runs {
		if r.Units[e.short(inst.address)] == nil || r.Run == e.runID {
			continue
		}
		what := cmp.Or(r.Task, r.Command)
		e.say("unit", inst.address, "output of %s in run %s at %s", what, r.Run, r.Started)
		err := eachEvent(filepath.Join(e.root, "runs", e.p.StackName, r.Run, "events.jsonl"), func(ev Event) error {
			if ev.Type == "log" && ev.Address == inst.address {
				e.replay(ev)
			}
			return nil
		})
		if err != nil {
			return failed(err), nil
		}
		return done, nil
	}
	e.say("unit", inst.address, "no run touched it yet")
	return done, nil
}

// replay hands a stored event to the caller as it was written, with its
// own time and run, and not to the record of this run, which would keep a
// second copy of it.
func (e *engine) replay(ev Event) {
	if e.opts.Emit != nil {
		e.opts.Emit(ev)
	}
}

// taskInputs are the inputs of the task name of a plugin: those of the
// plugin and its own.
func taskInputs(pl *model.Plugin, name string) map[string]*model.Input {
	in := maps.Clone(pl.Inputs)
	if in == nil {
		in = map[string]*model.Input{}
	}
	maps.Copy(in, pl.Tasks[name].Inputs)
	return in
}
