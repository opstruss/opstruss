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
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/state"
)

// Outputs evaluates the outputs of the project from the stored outputs of
// its units, as a snapshot read without a lock. A sensitive output, one
// declared so or read from a sensitive value, is secret.Mask unless
// opts.ShowSensitive, which refuses a project that has none.
func Outputs(opts Options) (map[string]any, error) {
	e, err := e0(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	out, marks := map[string]any{}, map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(e.p.Outputs)) {
		s := e.frameScope(e.top, e.rootUnits(), e.p.DirOf("outputs."+name), "", nil, nil)
		if o := e.p.Outputs[name]; !opts.ShowSensitive && (o.Sensitive || expr.MarksOf(o.Value, s) != nil) {
			// It shows as *** anyway, and what it reads may need a secret,
			// which only a run resolves.
			out[name], marks[name] = nil, true
			continue
		}
		v, m, err := expr.EvalMarked(e.p.Outputs[name].Value, s)
		if err != nil {
			return nil, e.redact.Err(fmt.Errorf("output %s: %w", name, err))
		}
		out[name] = v
		if m != nil || e.p.Outputs[name].Sensitive || e.redact.Contains(v) {
			marks[name] = true
		}
	}
	if !opts.ShowSensitive {
		return e.shown(out, marks), nil
	}
	if len(marks) == 0 {
		return nil, errors.New("--show-sensitive shows sensitive outputs, and the project has none")
	}
	return out, nil
}

// e0 reads the project with the instances of its units and their records,
// for a command that only reads.
func e0(opts Options) (*engine, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	e.pass = pass{}
	if err := e.selectUnits(false); err != nil {
		e.releaseSources()
		return nil, err
	}
	e.previewQuiet()
	return e, nil
}

// UnitStatus is a unit of the stack as its record has it.
type UnitStatus struct {
	Address string         `json:"address"`
	Kind    string         `json:"kind"`
	State   string         `json:"state"` // up, pending, or absent; for a service, as its status says
	Run     string         `json:"run,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
}

// TaskStatus is a task of the project with its last run.
type TaskStatus struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Internal    bool     `json:"internal,omitempty"`
	Triggers    []string `json:"triggers,omitempty"`
	Next        string   `json:"next,omitempty"` // when its schedule or every comes next, while the daemon runs
	Run         string   `json:"run,omitempty"`
	Via         string   `json:"via,omitempty"` // the task of that run, or its command, when the task ran inside it
	Outcome     string   `json:"outcome,omitempty"`
	Ended       string   `json:"ended,omitempty"`
}

// triggerNames say what the triggers of a task are, as list shows them.
func triggerNames(on *model.Triggers) []string {
	if on == nil {
		return nil
	}
	var out []string
	for _, s := range on.Schedule {
		out = append(out, (&Trigger{Kind: "schedule", Cron: s.Cron, Timezone: s.Timezone}).String())
	}
	for _, every := range on.Every {
		out = append(out, "every "+every)
	}
	for _, w := range on.Webhook {
		out = append(out, "webhook "+cmp.Or(w.Method, "POST")+" "+w.Path)
	}
	for range on.Watch {
		out = append(out, "watch")
	}
	for _, a := range on.After {
		out = append(out, "after "+a.Task)
	}
	return out
}

// Status is the content of a stack in one view, as opstruss list shows it.
type Status struct {
	Stack string        `json:"stack"`
	Tasks []*TaskStatus `json:"tasks"`
	Units []*UnitStatus `json:"units"`
}

// List reads the tasks of the project with their last runs, and the units
// with their state.
func List(opts Options) (*Status, error) {
	e, err := e0(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	st := &Status{Stack: e.p.StackName, Tasks: []*TaskStatus{}, Units: []*UnitStatus{}}
	runs, err := runRecords(e.root, e.p.StackName)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(e.p.Tasks)) {
		t := &TaskStatus{Name: name, Description: e.p.Tasks[name].Description, Internal: e.p.Tasks[name].Internal,
			Triggers: triggerNames(e.p.Tasks[name].On)}
		if r, via := lastRun(runs, name); r != nil {
			t.Run, t.Via, t.Outcome, t.Ended = r.Run, via, r.Outcome, r.Ended
			if via != "" {
				t.Outcome = r.Tasks[name].Outcome
			}
		}
		st.Tasks = append(st.Tasks, t)
	}
	for _, inst := range e.insts {
		u := &UnitStatus{Address: inst.address, Kind: "run", State: "absent"}
		if inst.def.Target != nil {
			u.Kind = inst.def.Target.Kind
		}
		if rec := inst.record; rec != nil && rec.Tombstone == nil {
			u.State, u.Run, u.Outputs = "up", rec.Run, e.shown(rec.Outputs, hiddenMarks(rec.Hidden[state.PartOutputs]))
			if rec.Pending != nil {
				u.State = "pending"
			}
		}
		if inst.def.Service != nil {
			u.Kind, u.State = "service", e.serviceStatus(inst)["state"].(string)
		}
		st.Units = append(st.Units, u)
	}
	return st, nil
}

// lastRun is the newest of runs, newest first, that ran the task name: as
// its task, or inside the run of another, which via names.
func lastRun(runs []*record.Record, name string) (r *record.Record, via string) {
	for _, r := range runs {
		if r.Command == "task" && r.Task == name {
			return r, ""
		}
		if r.Tasks[name] != nil {
			return r, cmp.Or(r.Task, r.Command)
		}
	}
	return nil, ""
}

// Runs reads the records of the runs of the stack, newest first.
func Runs(opts Options) ([]*record.Record, error) {
	p, err := project(opts)
	if err != nil {
		return nil, err
	}
	if opts.Model == nil {
		defer p.Close()
	}
	return runRecords(filepath.Join(p.Dir, ".opstruss"), p.StackName)
}

func runRecords(root, stack string) ([]*record.Record, error) {
	base := filepath.Join(root, "runs", stack)
	ents, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return []*record.Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []*record.Record{}
	for i := len(ents) - 1; i >= 0; i-- {
		if !ents[i].IsDir() {
			continue
		}
		dir := filepath.Join(base, ents[i].Name())
		rec, err := record.Read(dir)
		var newer *state.NewerError
		switch {
		case err == nil:
			out = append(out, rec)
		case errors.Is(err, fs.ErrNotExist):
			// A run that starts writes its record in a moment, and one
			// that retention dropped meanwhile is gone.
		case errors.As(err, &newer):
			return nil, err
		default:
			return nil, fmt.Errorf("%w; repair it, or remove %s to drop that run", err, dir)
		}
	}
	// Names order runs by the second they started in; within one, the
	// time they started says which is newer.
	started := func(r *record.Record) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, r.Started)
		return t
	}
	slices.SortStableFunc(out, func(a, b *record.Record) int { return started(b).Compare(started(a)) })
	return out, nil
}

// Events hands the stored events of the run id to emit, in order; with
// opts.Follow, also what a run that still runs writes, until it ends or
// ctx does.
func Events(ctx context.Context, opts Options, id string, emit func(Event)) error {
	p, err := project(opts)
	if err != nil {
		return err
	}
	if opts.Model == nil {
		defer p.Close()
	}
	err = record.ReadRun(ctx, filepath.Join(p.Dir, ".opstruss", "runs", p.StackName, filepath.Base(id)), opts.Follow, emit)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stack %s has no run %s", p.StackName, id)
	}
	return err
}
