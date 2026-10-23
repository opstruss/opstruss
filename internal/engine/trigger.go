package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
)

// Trigger is a trigger of a task of the project as a daemon installs it.
type Trigger struct {
	Task  string
	Kind  string // schedule or every
	Index int    // among the triggers of its kind in the task, in Model
	// Key names it apart from the place it has in the list: the hash of
	// its cron and time zone, by which a persistent schedule keeps when it
	// last ran, of its interval, of its method and path, of the task and
	// unit it comes after, or of the source of a watch. Triggers of a task
	// that are alike have the same Key.
	Key        string
	Cron       string
	Timezone   string // empty is the time zone of the machine
	Persistent bool
	Every      time.Duration

	// A webhook: where it listens, its secret as the trigger was
	// installed, how the secret is checked, and how it answers.
	Path      string
	Method    string
	Secret    string
	Signature model.Signature
	Respond   string

	// A watch: its source, a unit and its stream, paths relative to Dir,
	// or a call, which WatchSource prepares; and how its events gather.
	Unit     string
	Stream   string
	Paths    []model.PathItem
	Dir      string
	Call     bool
	Batch    time.Duration
	Debounce time.Duration

	// An after: the task, and the unit it runs for, whose runs that end
	// with Outcome make one of this task.
	After   string
	Outcome string
}

// TriggerEvent is what makes a run: where it comes from, as event.trigger
// says, its logical time, event.time, and the other fields of event.
// Simulated says that the caller wrote it, as --event, for a run of the
// command line that its record keeps as such.
type TriggerEvent struct {
	Trigger   string
	Time      time.Time
	Fields    map[string]any
	Simulated bool
}

// Triggers returns the schedule, every, and webhook triggers of the tasks
// of the project, as a daemon installs them, and the address the webhooks
// listen on. The secret of a webhook is evaluated then, with the sources
// of plan time and the secrets.
func Triggers(opts Options) ([]*Trigger, string, error) {
	e, err := load(opts)
	if err != nil {
		return nil, "", err
	}
	e.ctx, e.trigger = context.Background(), true
	var scope *expr.Scope
	installScope := func() (*expr.Scope, error) {
		if scope == nil {
			e.pass = pass{}
			if err := e.selectUnits(false); err != nil {
				return nil, err
			}
			e.previewQuiet()
			scope = e.triggerScope()
		}
		return scope, nil
	}
	var out, later []*Trigger
	for _, name := range slices.Sorted(maps.Keys(e.p.Tasks)) {
		ts, err := triggersOf(e.p, name)
		if err != nil {
			return nil, "", err
		}
		for _, t := range ts {
			switch t.Kind {
			case "webhook":
				s, err := installScope()
				if err != nil {
					return nil, "", err
				}
				if t.Secret, err = expr.EvalText(e.p.Tasks[name].On.Webhook[t.Index].Secret, s); err != nil {
					return nil, "", e.redact.Err(fmt.Errorf("task %s: webhook secret: %w", name, err))
				}
			case "watch":
				w := e.p.Tasks[name].On.Watch[t.Index]
				for j, it := range t.Paths {
					if !expr.HasExpr(it.Pattern) {
						continue
					}
					s, err := installScope()
					if err != nil {
						return nil, "", err
					}
					if t.Paths[j].Pattern, err = expr.EvalText(it.Pattern, s); err != nil {
						return nil, "", e.redact.Err(fmt.Errorf("task %s: watch: paths: %w", name, err))
					}
				}
				t.Key = keyOf(name, t.Unit, t.Stream, t.Dir, expr.JSON(t.Paths), expr.JSON(w.Calls))
			}
			if t.Kind == "watch" || t.Kind == "after" {
				later = append(later, t)
			} else {
				out = append(out, t)
			}
		}
	}
	out = append(out, later...)
	listen := ""
	if e.p.Runtime != nil {
		listen = e.p.Runtime.Listen
	}
	return out, listen, nil
}

// triggersOf gives the triggers of the task name of p as its file writes
// them, before what an install evaluates: the secret of a webhook, and the
// expressions in the paths of a watch, by which its Key changes.
func triggersOf(p *model.Project, name string) ([]*Trigger, error) {
	on := p.Tasks[name].On
	if on == nil {
		return nil, nil
	}
	var out []*Trigger
	for i, s := range on.Schedule {
		out = append(out, &Trigger{Task: name, Kind: "schedule", Index: i, Key: keyOf(s.Cron, s.Timezone),
			Cron: s.Cron, Timezone: s.Timezone, Persistent: s.Persistent})
	}
	for i, every := range on.Every {
		d, err := time.ParseDuration(every)
		if err != nil {
			return nil, fmt.Errorf("task %s: every: %w", name, err)
		}
		out = append(out, &Trigger{Task: name, Kind: "every", Index: i, Key: keyOf(every), Every: d})
	}
	for i, w := range on.Webhook {
		method := cmp.Or(w.Method, "POST")
		out = append(out, &Trigger{Task: name, Kind: "webhook", Index: i, Key: keyOf(method, w.Path), Path: w.Path,
			Method: method, Signature: *w.Signature, Respond: cmp.Or(w.Respond, "accepted")})
	}
	for i, w := range on.Watch {
		t := &Trigger{Task: name, Kind: "watch", Index: i, Unit: w.Unit, Stream: cmp.Or(w.Stream, "both"), Paths: slices.Clone(w.Paths),
			Dir: p.DirOf("tasks." + name), Call: len(w.Calls) > 0}
		t.Key = keyOf(name, t.Unit, t.Stream, t.Dir, expr.JSON(t.Paths), expr.JSON(w.Calls))
		for _, d := range []struct {
			text string
			to   *time.Duration
		}{{w.Batch, &t.Batch}, {w.Debounce, &t.Debounce}} {
			if d.text != "" {
				var err error
				if *d.to, err = time.ParseDuration(d.text); err != nil {
					return nil, fmt.Errorf("task %s: watch: %w", name, err)
				}
			}
		}
		out = append(out, t)
	}
	for i, a := range on.After {
		out = append(out, &Trigger{Task: name, Kind: "after", Index: i, Key: keyOf(a.Task, a.Unit), After: a.Task,
			Unit: a.Unit, Outcome: cmp.Or(a.Outcome, "success")})
	}
	return out, nil
}

// WithError is a with: of a trigger whose values the inputs of its task do
// not take, so no run is made; a webhook answers it with 422.
type WithError struct{ Err error }

func (e *WithError) Error() string { return e.Err.Error() }
func (e *WithError) Unwrap() error { return e.Err }

func keyOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Fire makes a run of the task of t for opts.Event: if: of the trigger
// decides whether there is one, and with: gives its inputs, checked
// against those of the task. It returns nil, and makes no run, when if:
// leaves the event out.
func Fire(ctx context.Context, opts Options, t *Trigger) (*Result, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	e.trigger = true
	return e.fire(ctx, t, nil, true)
}

// fire makes a run of the task of t for e.event, as Fire does, with over
// in place of what with: gives for the same inputs; if: of t decides only
// when ask says so.
func (e *engine) fire(ctx context.Context, t *Trigger, over map[string]any, ask bool) (*Result, error) {
	task := e.p.Tasks[t.Task]
	if task == nil || task.On == nil {
		return nil, fmt.Errorf("the project has no task %s with triggers", t.Task)
	}
	var cond any
	var with map[string]any
	switch {
	case t.Kind == "schedule" && t.Index < len(task.On.Schedule):
		cond, with = task.On.Schedule[t.Index].If, task.On.Schedule[t.Index].With
	case t.Kind == "webhook" && t.Index < len(task.On.Webhook):
		cond, with = task.On.Webhook[t.Index].If, task.On.Webhook[t.Index].With
	case t.Kind == "watch" && t.Index < len(task.On.Watch):
		// if: chose each event before they gathered into this run.
		with = task.On.Watch[t.Index].With
	case t.Kind == "after" && t.Index < len(task.On.After):
		cond, with = task.On.After[t.Index].If, task.On.After[t.Index].With
	}
	e.ctx = ctx
	if !ask {
		cond = nil
	}
	given := map[string]any{}
	if cond != nil || with != nil {
		e.pass = pass{}
		if err := e.selectUnits(false); err != nil {
			return nil, err
		}
		e.previewQuiet()
		s := e.triggerScope()
		if cond != nil {
			ok, err := expr.EvalCondition(cond, s)
			if err != nil {
				return nil, e.redact.Err(fmt.Errorf("if: %w", err))
			}
			if !ok {
				return nil, nil
			}
		}
		if with != nil {
			v, marks, err := expr.EvalMarked(anyMap(with), s)
			if err != nil {
				return nil, e.redact.Err(fmt.Errorf("with: %w", err))
			}
			e.hide(v, marks)
			given, _ = v.(map[string]any)
		}
		e.resetUnits()
	}
	if with != nil || over != nil {
		given = maps.Clone(given)
		if given == nil {
			given = map[string]any{}
		}
		maps.Copy(given, over)
		if _, err := conform(task.Inputs, given); err != nil {
			return nil, &WithError{e.redact.Err(fmt.Errorf("with: %w", err))}
		}
	}
	if err := e.start("task", t.Task, over, t); err != nil {
		return nil, err
	}
	return e.task(e.ctx, t.Task, given), nil
}

// triggerScope is what if: and with: of a trigger read: the sources of
// plan time, the secrets, and event.
func (e *engine) triggerScope() *expr.Scope {
	f, k := e.top, e.newKV()
	units, unitMarks := e.unitsView(f, slices.Sorted(maps.Keys(e.p.Units)))
	s := &expr.Scope{Dir: e.p.Dir, Vars: map[string]any{"stack": e.p.StackName, "inputs": e.inputs,
		"locals": k.localsOf(f, e.inputs), "opstruss": e.facts(nil), "units": units, "secrets": e.secrets, "event": e.event},
		Marks: map[string]any{}}
	if len(e.secrets) > 0 {
		s.Marks["secrets"] = true
	}
	for name, m := range map[string]map[string]any{"inputs": e.inputMarks, "locals": e.localMarksOf(f, e.inputMarks, k), "units": unitMarks} {
		if len(m) > 0 {
			s.Marks[name] = m
		}
	}
	return s.WithKV(k.get, k.marked)
}

// eventOf is event of a run made by ev, or by the command line.
func eventOf(ev *TriggerEvent, started time.Time) map[string]any {
	if ev == nil {
		return map[string]any{"trigger": "cli", "time": started}
	}
	out := maps.Clone(ev.Fields)
	if out == nil {
		out = map[string]any{}
	}
	out["trigger"], out["time"] = ev.Trigger, ev.Time
	return out
}

// String says what t is, as list and the log of the daemon show it.
func (t *Trigger) String() string {
	switch t.Kind {
	case "webhook":
		return "webhook " + t.Method + " " + t.Path
	case "watch":
		switch {
		case t.Unit != "":
			return "watch " + t.Unit
		case len(t.Paths) > 0:
			return "watch paths"
		}
		return "watch"
	case "after":
		return "after " + t.After
	case "schedule":
		if t.Timezone != "" {
			return fmt.Sprintf("schedule %s (%s)", t.Cron, t.Timezone)
		}
		return "schedule " + t.Cron
	}
	return "every " + t.Every.String()
}
