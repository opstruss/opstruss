package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// Logs shows the output of the units: the log of a service, the logs task
// of a plugin that has one, or the output of the last run that touched a
// unit, as docker logs shows that of a container that ended; and that of
// the tasks named, from their last run. With opts.Follow, the logs of
// services, the plugins that follow, and the runs of the tasks go on
// until ctx ends. The outcome of the runs shown is not that of logs.
func Logs(ctx context.Context, opts Options) (*Result, error) {
	e, err := open(ctx, opts, "logs", "")
	if err != nil {
		return nil, err
	}
	// A task is named as it is; a unit by a name or a pattern. A name of
	// both shows both.
	var units, tasks []string
	unknown := false
	for _, n := range opts.Units {
		_, task := e.p.Tasks[n]
		unit := e.p.Units[n] != nil || strings.ContainsAny(n, "*[/")
		if task {
			tasks = append(tasks, n)
		}
		if unit || !task {
			units = append(units, n)
		}
		if !unit && !task {
			e.fail("", fmt.Errorf("no unit or task matches %s", n))
			unknown = true
		}
	}
	var wg sync.WaitGroup
	show := func(fn func()) {
		switch {
		case unknown:
		case opts.Follow:
			wg.Go(fn)
		default:
			fn()
		}
	}
	if len(units) > 0 || len(tasks) == 0 {
		show(func() {
			got, _, err := e.logsPass(ctx, units, opts.Follow)
			if err != nil {
				e.fail("", err)
			}
			for _, u := range got {
				if u.(map[string]any)["outcome"] == failure {
					e.mu.Lock()
					e.exit = max(e.exit, 1)
					e.mu.Unlock()
				}
			}
		})
	}
	for _, name := range tasks {
		show(func() {
			if err := e.taskLogs(ctx, name, opts.Follow); err != nil {
				e.fail("", err)
			}
		})
	}
	wg.Wait()
	e.close(map[int]string{0: success, 1: failure}[e.exit], nil, nil)
	return e.result(), nil
}

// taskLogs shows the output of the last run of the task name, with the
// rule of list, and with follow that run until it ends, then each later
// run of the task as its task, until ctx ends, as journalctl -f -u does.
// Runs are found in runs/, so those of the daemon and of other terminals
// alike.
func (e *engine) taskLogs(ctx context.Context, name string, follow bool) error {
	runs, err := runRecords(e.root, e.p.StackName)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.Run] = true
	}
	if r, via := lastRun(runs, name); r != nil {
		if err := e.showRun(ctx, name, r, via, follow); err != nil {
			return err
		}
	} else {
		e.sayTask(name, "", "no run of it yet")
	}
	if !follow {
		return nil
	}
	base := filepath.Join(e.root, "runs", e.p.StackName)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(record.FollowEvery):
		}
		ents, err := os.ReadDir(base)
		if err != nil {
			return err
		}
		for _, ent := range ents {
			if seen[ent.Name()] {
				continue
			}
			// A run writes its record as it starts; until then it is
			// looked at again.
			r, err := record.Read(filepath.Join(base, ent.Name()))
			if err != nil {
				continue
			}
			seen[r.Run] = true
			if r.Command == "task" && r.Task == name {
				if err := e.showRun(ctx, name, r, "", true); err != nil {
					return err
				}
			}
		}
	}
}

// showRun shows the run r of the task name after a line that names it:
// all its events, or when the task ran inside the run of another, which
// via names, the events of that task; with follow, until the run ends.
func (e *engine) showRun(ctx context.Context, name string, r *record.Record, via string, follow bool) error {
	head := "run " + r.Run + " at " + r.Started
	if via != "" {
		head += " via " + via
	}
	e.sayTask(name, "", "%s", head)
	err := record.ReadRun(ctx, filepath.Join(e.root, "runs", e.p.StackName, r.Run), follow, func(ev Event) {
		if via == "" || ev.Task == name {
			e.replay(ev)
		}
	})
	if errors.Is(err, fs.ErrNotExist) {
		// Retention dropped it meanwhile.
		return nil
	}
	return err
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
		err := record.ReadRun(ctx, filepath.Join(e.root, "runs", e.p.StackName, r.Run), false, func(ev Event) {
			if ev.Type == "log" && ev.Address == inst.address {
				e.replay(ev)
			}
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
