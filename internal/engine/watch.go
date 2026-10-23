package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// A run that a watch with batch or debounce makes carries at most MaxItems
// lines or paths, and MaxBytes of them as JSON; the events that do not fit
// are only counted.
const (
	MaxItems = 10000
	MaxBytes = 1 << 20
)

// Batch gathers the events of a watch with batch or debounce into the
// event of one run as they come, in bounded memory: event.lines or
// event.paths keep the first that fit, a file that changes again stays
// with what happened to it first, and event.truncated counts the events
// that did not fit, as truncatedAlerts does in Alertmanager. A file that
// changes again after the list is full is counted again, since only the
// files in the list are remembered.
type Batch struct {
	tr        *Trigger
	items     []any
	seen      map[string]bool // the paths in items
	size      int
	truncated int64
	full      bool
}

// NewBatch returns an empty Batch for the watch tr.
func NewBatch(tr *Trigger) *Batch { return &Batch{tr: tr, seen: map[string]bool{}} }

// Add gathers ev, one event of the source of the watch.
func (b *Batch) Add(ev map[string]any) {
	item, path := ev["line"], ""
	if len(b.tr.Paths) > 0 {
		item = ev["paths"].([]any)[0]
		path, _ = item.(map[string]any)["path"].(string)
		if b.seen[path] {
			return
		}
	}
	if !b.full {
		data, _ := json.Marshal(item)
		if len(b.items) < MaxItems && b.size+len(data) <= MaxBytes {
			b.items = append(b.items, item)
			b.size += len(data)
			if len(b.tr.Paths) > 0 {
				b.seen[path] = true
			}
			return
		}
		b.full = true
	}
	b.truncated++
}

// Empty reports whether b has gathered nothing.
func (b *Batch) Empty() bool { return len(b.items) == 0 && b.truncated == 0 }

// Event is the event of the run that b makes: lines or paths, count, which
// is both, and truncated, and for the lines of a unit its unit and stream.
func (b *Batch) Event() map[string]any {
	key := "lines"
	if len(b.tr.Paths) > 0 {
		key = "paths"
	}
	items := slices.Clone(b.items)
	if items == nil {
		items = []any{}
	}
	out := map[string]any{key: items, "count": int64(len(b.items)) + b.truncated, "truncated": b.truncated}
	if b.tr.Unit != "" {
		out["unit"], out["stream"] = b.tr.Unit, b.tr.Stream
	}
	return out
}

// Filter decides for each event of a watch whether it goes on, by the if:
// of the trigger, with the project read once.
type Filter struct {
	e    *engine
	cond any
}

// WatchKind says where the events of the watch t of p come from: lines,
// the lines of a service; paths, changes of files; or program, the lines
// of a program that it runs, a call or the watch hook of the plugin of a
// unit.
func WatchKind(p *model.Project, t *Trigger) string {
	switch u := p.Units[t.Unit]; {
	case t.Unit != "" && u != nil && u.Service != nil:
		return "lines"
	case len(t.Paths) > 0:
		return "paths"
	}
	return "program"
}

// NewFilter reads the project of opts for the if: of the watch t.
func NewFilter(opts Options, t *Trigger) (*Filter, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
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

// Match reports whether the event with fields, which came at at, goes on.
func (f *Filter) Match(fields map[string]any, at time.Time) (bool, error) {
	if f.cond == nil {
		return true, nil
	}
	f.e.event = eventOf(&TriggerEvent{Trigger: "watch", Time: at, Fields: fields}, time.Time{})
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
	defer e.releaseSources()
	e.ctx, e.trigger = context.Background(), true
	e.pass = pass{}
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewQuiet()
	run := e.outside()
	prepare := func(c *plugin.Call) (*plugin.Program, error) {
		p, err := run.Prepare(c, dir)
		if err == nil {
			p.ReleaseSources = e.p.Sources.Retain()
		}
		return p, err
	}
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
		return prepare(&plugin.Call{Command: cmd, Shell: hook.Shell, Base: inst.dir, Dir: r.dir, Env: r.plugin.env,
			Pass: slices.Sorted(maps.Keys(r.pass)), Inputs: r.plugin.inputs, PluginDir: r.plugin.dir,
			Unit: &plugin.Unit{Address: inst.address}})
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
		return prepare(&plugin.Call{Command: cmd, Base: base})
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
		return prepare(&plugin.Call{Command: cmd, Shell: task.Shell, Base: base, Dir: task.Dir, Env: values,
			Pass: prog.pass, Inputs: in, PluginDir: target.Package.Dir})
	}
	return nil, fmt.Errorf("%s is no program that writes a line per event", name)
}
