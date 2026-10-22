package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
)

// Trigger is a trigger of a task of the project as a daemon installs it.
type Trigger struct {
	Task  string
	Kind  string // schedule or every
	Index int    // among the triggers of its kind in the task
	// Key names it apart from the place it has in the list: the hash of
	// its cron and time zone, or of its interval, by which a persistent
	// schedule keeps when it last ran.
	Key        string
	Cron       string
	Timezone   string // empty is the time zone of the machine
	Persistent bool
	Every      time.Duration
}

// TriggerEvent is what makes a run: where it comes from, as event.trigger
// says, its logical time, event.time, and the other fields of event.
type TriggerEvent struct {
	Trigger string
	Time    time.Time
	Fields  map[string]any
}

// Triggers returns the schedule and every triggers of the tasks of the
// project, as a daemon installs them.
func Triggers(opts Options) ([]*Trigger, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	var out []*Trigger
	for _, name := range slices.Sorted(maps.Keys(e.p.Tasks)) {
		on := e.p.Tasks[name].On
		if on == nil {
			continue
		}
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
	}
	return out, nil
}

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
	task := e.p.Tasks[t.Task]
	if task == nil || task.On == nil {
		return nil, fmt.Errorf("the project has no task %s with triggers", t.Task)
	}
	var cond any
	var with map[string]any
	if t.Kind == "schedule" && t.Index < len(task.On.Schedule) {
		cond, with = task.On.Schedule[t.Index].If, task.On.Schedule[t.Index].With
	}
	e.ctx = ctx
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
			if _, err := conform(task.Inputs, given); err != nil {
				return nil, e.redact.Err(fmt.Errorf("with: %w", err))
			}
		}
		e.resetUnits()
	}
	if err := e.start("task", t.Task); err != nil {
		return nil, err
	}
	return e.task(ctx, t.Task, given), nil
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
	case "schedule":
		if t.Timezone != "" {
			return fmt.Sprintf("schedule %s (%s)", t.Cron, t.Timezone)
		}
		return "schedule " + t.Cron
	}
	return "every " + t.Every.String()
}
