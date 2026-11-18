package engine

import (
	"maps"
	"slices"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// Follower is a run that a run that ended makes through after:: its
// trigger, the fields of its event, and the stack it runs in, which is
// GlobalScope for a global task.
type Follower struct {
	Trigger *Trigger
	Fields  map[string]any
	Stack   string
}

// AfterTriggers are the after triggers of the tasks of p, in order of task.
func AfterTriggers(p *model.Project) []*Trigger {
	var out []*Trigger
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		ts, _ := triggersOf(p, name)
		for _, t := range ts {
			if t.Kind == "after" {
				out = append(out, t)
			}
		}
	}
	return out
}

// Match is the one place that decides which runs a run that ended makes:
// the triggers of ts that follow the root task or command of rec, the
// unit it names among those rec touched, and its outcome. A task of the
// stack follows only the runs of that stack; a global task those of every
// scope, and runs in GlobalScope. global says which tasks are global.
func Match(ts []*Trigger, rec *record.Record, global func(task string) bool) []*Follower {
	if rec == nil {
		return nil
	}
	name := rec.Task
	if name == "" {
		name = rec.Command
	}
	stack := rec.Stack
	if stack == model.GlobalScope {
		stack = ""
	}
	// event.outputs has the value of each output; a sensitive one has none
	// in the record, so the run after it gets none.
	outputs := map[string]any{}
	for k, o := range rec.Outputs {
		if v, ok := record.OutputValue(o); ok {
			outputs[k] = v
		}
	}
	var out []*Follower
	for _, t := range ts {
		if t.After != name || (t.Unit != "" && rec.Units[t.Unit] == nil) || (t.Outcome != "any" && t.Outcome != rec.Outcome) {
			continue
		}
		where := rec.Stack
		if global(t.Task) {
			where = model.GlobalScope
		} else if rec.Stack == model.GlobalScope {
			continue // a task of a stack follows only the runs of its stack
		}
		fields := map[string]any{"task": name, "outcome": rec.Outcome, "run": rec.Run, "stack": stack}
		if name != "init" {
			fields["unit"], fields["outputs"] = t.Unit, outputs
		}
		out = append(out, &Follower{Trigger: t, Fields: fields, Stack: where})
	}
	return out
}

// followers are the runs that this run makes through after:, once its
// record is finished.
func (e *engine) followers(rec *record.Record) []*Follower {
	p := e.p
	return Match(AfterTriggers(p), rec, func(task string) bool { return p.Tasks[task] != nil && p.Tasks[task].Global })
}
