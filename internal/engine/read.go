package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/record"
)

// Outputs evaluates the outputs of the project from the stored outputs of
// its units, as a snapshot read without a lock.
func Outputs(opts Options) (map[string]any, error) {
	e, err := e0(opts)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(e.p.Outputs)) {
		s := e.scope(nil, e.p.DirOf("outputs."+name))
		v, err := expr.EvalValue(e.p.Outputs[name].Value, s)
		if err != nil {
			return nil, fmt.Errorf("output %s: %w", name, err)
		}
		out[name] = v
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
		return nil, err
	}
	e.previewQuiet()
	return e, nil
}

// UnitStatus is a unit of the stack as its record has it.
type UnitStatus struct {
	Address string         `json:"address"`
	Kind    string         `json:"kind"`
	State   string         `json:"state"` // up, pending, or absent
	Run     string         `json:"run,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
}

// TaskStatus is a task of the project with its last run.
type TaskStatus struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Internal    bool   `json:"internal,omitempty"`
	Run         string `json:"run,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Ended       string `json:"ended,omitempty"`
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
	st := &Status{Stack: e.p.StackName, Tasks: []*TaskStatus{}, Units: []*UnitStatus{}}
	runs, err := runRecords(e.root, e.p.StackName)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(e.p.Tasks)) {
		t := &TaskStatus{Name: name, Description: e.p.Tasks[name].Description, Internal: e.p.Tasks[name].Internal}
		for _, r := range runs {
			if r.Command == "task" && r.Task == name {
				t.Run, t.Outcome, t.Ended = r.Run, r.Outcome, r.Ended
				break
			}
		}
		st.Tasks = append(st.Tasks, t)
	}
	for _, inst := range e.insts {
		u := &UnitStatus{Address: inst.address, Kind: "run", State: "absent"}
		if rec := inst.record; rec != nil && rec.Tombstone == nil {
			u.State, u.Run, u.Outputs = "up", rec.Run, rec.Outputs
			if rec.Pending != nil {
				u.State = "pending"
			}
		}
		st.Units = append(st.Units, u)
	}
	return st, nil
}

// Runs reads the records of the runs of the stack, newest first.
func Runs(opts Options) ([]*record.Record, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	return runRecords(e.root, e.p.StackName)
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
		if rec, err := record.Read(filepath.Join(base, ents[i].Name())); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Events hands the stored events of the run id to emit, in order.
func Events(opts Options, id string, emit func(Event)) error {
	e, err := load(opts)
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(e.root, "runs", e.p.StackName, filepath.Base(id), "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stack %s has no run %s", e.p.StackName, id)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return err
		}
		emit(ev)
	}
	return sc.Err()
}
