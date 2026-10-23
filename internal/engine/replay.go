package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/secret"
)

// replayVersion is the version of the shape of a replay payload.
const replayVersion = 1

// maxReplay bounds a replay payload: a call of the engine API carries no
// more.
const maxReplay = 16 << 20

// replay is what a run got from outside the project, which runs rerun
// gives again: --input, the inputs and flags of its caller, and the fields
// of its event. It is sealed whole for the age key of the machine, since a
// webhook or a flag may carry a secret that nothing marks, and the runs it
// lives with stay on this machine too; the run it belongs to is inside.
type replay struct {
	V       int               `json:"v"`
	Project string            `json:"project"`
	Stack   string            `json:"stack"`
	Run     string            `json:"run"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Flags   map[string]any    `json:"flags,omitempty"`
	With    map[string]any    `json:"with,omitempty"`
	Event   map[string]any    `json:"event,omitempty"`
}

func (rp *replay) empty() bool {
	return len(rp.Inputs)+len(rp.Flags)+len(rp.With)+len(rp.Event) == 0
}

// rerunnable are the commands whose runs run again.
var rerunnable = []string{"task", "up", "down"}

// replayOf is what rec keeps to run the run again, and the payload to seal
// beside it, of a run of command with the inputs with, made by the
// trigger tr or the command line.
func (e *engine) replayOf(command string, with map[string]any, tr *Trigger) (*record.Replay, *replay) {
	if !slices.Contains(rerunnable, command) {
		return nil, nil
	}
	out := &record.Replay{}
	rp := &replay{V: replayVersion, Inputs: e.opts.Inputs, Flags: e.opts.Flags, With: with}
	if ev := e.opts.Event; ev != nil && tr != nil {
		out.Trigger, out.Time = fileKey(e, tr), ev.Time.UTC().Format(time.RFC3339Nano)
		rp.Event = ev.Fields
	}
	if rp.empty() {
		return out, nil
	}
	out.Payload, out.Error = record.Unavailable, "the run ended before it kept its replay payload"
	return out, rp
}

// fileKey is the key of tr as the project file writes it, before what an
// install evaluates, so a later file finds it again.
func fileKey(e *engine, tr *Trigger) string {
	all, _ := triggersOf(e.p, tr.Task)
	for _, t := range all {
		if t.Kind == tr.Kind && t.Index == tr.Index {
			return t.Key
		}
	}
	return tr.Key
}

// keepReplay seals rp for the age key of this machine beside the record,
// without the data key of the stack, which belongs to its state; without
// an age key, the run goes on and its record says why it cannot run
// again. Nothing is ever kept in plain text.
func (e *engine) keepReplay(rp *replay) {
	err := func() error {
		mine, _, err := e.machineKey()
		if err != nil {
			return fmt.Errorf("this machine has no age key: %w", err)
		}
		rp.Project, rp.Stack, rp.Run = e.p.Name, e.p.StackName, e.runID
		data, err := json.Marshal(rp)
		if err != nil {
			return err
		}
		if len(data) > maxReplay {
			return fmt.Errorf("they take %d bytes, more than %d", len(data), maxReplay)
		}
		sealed, err := secret.SealAge(data, []string{mine.Recipient()})
		if err != nil {
			return err
		}
		return e.rec.WriteReplay([]byte(sealed))
	}()
	uerr := e.rec.Update(func(r *record.Record) {
		r.Replay.Payload, r.Replay.Error = record.Sealed, ""
		if err != nil {
			r.Replay.Payload, r.Replay.Error = record.Unavailable, e.redact.Log(err.Error())
		}
	})
	if err == nil {
		err = uerr
	}
	if err != nil {
		e.warn("", "runs rerun cannot run this run again, since its inputs and event were not kept: %v", e.redact.Err(err))
	}
}

// Rerun runs the run id of the stack again: its command, task, and units,
// with what came from outside the project as it ran, --input, the inputs
// and flags of its caller, and its event, through the project as it is
// now. The approval of opts is asked again, and if: of its trigger is not:
// with: gives the inputs again from the event, the secrets, and the state
// of now. A run that cannot run again fails before anything runs.
func Rerun(ctx context.Context, opts Options, id string) (*Result, error) {
	// The inputs come from the run, so the project is read without them.
	rec, dir, err := runOf(opts, id)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(rerunnable, rec.Command) {
		return nil, fmt.Errorf("runs rerun runs tasks, up, and down again; run %s is %s", rec.Run, rec.Command)
	}
	if rec.Replay == nil {
		return nil, fmt.Errorf("run %s cannot run again: it kept nothing for runs rerun, as an older OpsTruss made it", rec.Run)
	}
	rp := &replay{}
	switch rec.Replay.Payload {
	case record.Unavailable:
		return nil, fmt.Errorf("run %s cannot run again: its inputs and event were not kept, since %s", rec.Run, rec.Replay.Error)
	case record.Sealed:
		if rp, err = openReplay(dir, rec, opts.LookupEnv); err != nil {
			return nil, fmt.Errorf("run %s cannot run again: %w", rec.Run, err)
		}
	}
	o := opts
	o.Inputs, o.Flags, o.Units, o.NoDeps, o.RerunOf = rp.Inputs, rp.Flags, rec.Args, rec.NoDeps, rec.Run
	var res *Result
	switch {
	case rec.Command != "task":
		res, err = runCommand(ctx, o, rec.Command)
	case rec.Replay.Trigger == "":
		res, err = Task(ctx, o, rec.Task, rp.With)
	default:
		res, err = rerunEvent(ctx, o, rec, rp)
	}
	if res != nil {
		res.Inputs = rp.Inputs
	}
	return res, err
}

// openReplay reads the sealed payload of the run rec in dir, and opens it
// with the age keys of this machine.
func openReplay(dir string, rec *record.Record, lookupEnv func(string) (string, bool)) (*replay, error) {
	data, err := record.ReadReplay(dir)
	if err != nil {
		return nil, fmt.Errorf("its inputs and event are gone: %w", err)
	}
	ids, err := secret.Identities(lookupEnv)
	if err != nil {
		return nil, err
	}
	plain, _, err := secret.OpenAge(string(data), ids, maxReplay)
	if errors.Is(err, secret.ErrNoIdentity) {
		return nil, errors.New("no age key of this machine opens its inputs and event, which the age key of the machine that ran it sealed")
	}
	if err != nil {
		return nil, fmt.Errorf("its inputs and event do not open: %w", err)
	}
	v, err := expr.FromJSON(plain)
	if err != nil {
		return nil, fmt.Errorf("its inputs and event do not read: %w", err)
	}
	m, _ := v.(map[string]any)
	rp := &replay{}
	if n, _ := m["v"].(int64); n != replayVersion {
		return nil, fmt.Errorf("its inputs and event are of version %v, which this OpsTruss does not read", m["v"])
	}
	if m["project"] != rec.Project || m["stack"] != rec.Stack || m["run"] != rec.Run {
		return nil, errors.New("its inputs and event belong to another run")
	}
	if in, ok := m["inputs"].(map[string]any); ok {
		rp.Inputs = map[string]string{}
		for k, x := range in {
			rp.Inputs[k], _ = x.(string)
		}
	}
	rp.Flags, _ = m["flags"].(map[string]any)
	rp.With, _ = m["with"].(map[string]any)
	rp.Event, _ = m["event"].(map[string]any)
	return rp, nil
}

// rerunEvent runs the task of rec again for its event: by the trigger of
// the same kind and key in the project as it is now, without its if:.
func rerunEvent(ctx context.Context, opts Options, rec *record.Record, rp *replay) (*Result, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	e.trigger = true
	kind := cmp.Or(rec.Simulated, rec.Trigger)
	var tr *Trigger
	if t := e.p.Tasks[rec.Task]; t != nil && t.On != nil {
		all, err := triggersOf(e.p, rec.Task)
		if err != nil {
			return nil, err
		}
		for _, t := range all {
			if t.Kind == kind && t.Key == rec.Replay.Trigger {
				tr = t
				break
			}
		}
	}
	if tr == nil {
		return nil, fmt.Errorf("run %s cannot run again: task %s has no longer the %s trigger whose event made it", rec.Run, rec.Task, kind)
	}
	at, _ := time.Parse(time.RFC3339Nano, rec.Replay.Time)
	e.opts.Event = &TriggerEvent{Trigger: kind, Time: at, Fields: rp.Event, Simulated: true}
	e.event = eventOf(e.opts.Event, e.started)
	return e.fire(ctx, tr, rp.With, false)
}
