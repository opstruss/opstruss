// Package engine runs a stack: the plan, up, and down of its units in
// graph order, each under the lock of its address, with what each unit did
// in its record.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/state"
)

// Options says what a run does.
type Options struct {
	Dir       string            // the project directory
	Cwd       string            // where OpsTruss started, before -C: opstruss.cwd
	Stack     string            // empty chooses the only stack
	Inputs    map[string]string // --input
	LookupEnv func(string) (string, bool)
	Environ   []string // the operator's environment
	Units     []string // names or patterns; none means every unit
	Yes       bool     // approve the changes of this run
	Force     bool     // up: run the units named although unchanged; down: although pass values differ
	NoDeps    bool     // leave out the dependencies of up and the dependents of down
	Emit      func(Event)
	// Approve asks whoever is at a terminal to approve one change, one
	// question at a time; without it, a change that needs approval stops
	// at plan. Ask asks for a required input that has no value.
	Approve func(context.Context, Question) (bool, error)
	Ask     func(context.Context, string, *model.Input) (string, error)
	Debug   bool // OPSTRUSS_LOG=debug: diagnostics of the engine as debug events
}

// Question is a change that waits for approval.
type Question struct {
	Address string `json:"address,omitempty"`
	Task    string `json:"task,omitempty"`
	Summary string `json:"summary"`
}

// Event is one thing that happened in a run; the last one, of type run,
// carries the outcome, the exit code, and the outputs of a task.
type Event = record.Event

// Result is how a run ended.
type Result struct {
	Run      string
	ExitCode int // 0 success, 1 failure, 2 stopped at plan for approval
}

// Outcomes of units, tasks, and steps.
const (
	success   = "success"
	failure   = "failure"
	skipped   = "skipped"
	cancelled = "cancelled"
)

// MaxDepth is how deep runs nest, through opstruss in a program of a run.
const MaxDepth = 3

// DefaultParallel is how many units run at once without OPSTRUSS_PARALLEL.
const DefaultParallel = 10

// DefaultTimeout bounds a unit's turn, from waiting for its lock to the end
// of its program.
const DefaultTimeout = 6 * time.Hour

type engine struct {
	opts     Options
	p        *model.Project
	inputs   map[string]any
	locals   map[string]any
	store    *state.Store
	root     string // the project's .opstruss
	runID    string
	parents  []string
	host     string
	holder   state.Holder
	started  time.Time
	run      *plugin.Run
	parallel int
	sel      *selection
	pass     pass
	passMu   sync.Mutex
	read     map[string]string // digests of the exact plans read in this run, by address; empty when not exact
	rec      *record.Run
	touched  map[string]*record.Unit

	rootTask string
	needed   map[string]*taskOnce               // tasks of needs, which run once per run
	holders  map[string]context.CancelCauseFunc // concurrency locks this process holds

	insts  []*instance // in graph order
	byUnit map[string][]*instance

	moveFrom   map[string]*state.Record // the record a target of moved: starts from
	moveFailed map[string]error         // targets whose move cannot happen
	moving     map[string]bool          // addresses that moved: moves away from

	mu       sync.Mutex
	exit     int
	promptMu sync.Mutex
	inPass   bool // a pass is expanding and planning units
}

// open reads the project and the state of its stack, marks the runs that
// a crash left running, and starts the record of a run of command, or of
// the task with that name.
func open(opts Options, command, task string) (*engine, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	return e, e.start(command, task)
}

// load reads the project and opens the state of its stack, for a run or
// for a command that only reads.
func load(opts Options) (*engine, error) {
	p, err := model.Load(opts.Dir, opts.Stack)
	if err != nil {
		return nil, err
	}
	values, err := p.ResolveInputs(opts.Inputs, opts.LookupEnv)
	if err != nil {
		return nil, err
	}
	inputs := map[string]any{}
	for k, v := range values {
		inputs[k] = v.Value
	}
	locals, err := p.EvalLocals(inputs)
	if err != nil {
		return nil, err
	}
	e := &engine{opts: opts, p: p, inputs: inputs, locals: locals, root: filepath.Join(p.Dir, ".opstruss"),
		started: time.Now().UTC(), parallel: DefaultParallel, byUnit: map[string][]*instance{},
		read: map[string]string{}, needed: map[string]*taskOnce{}, holders: map[string]context.CancelCauseFunc{},
		touched: map[string]*record.Unit{}}
	if s, ok := opts.LookupEnv("OPSTRUSS_PARALLEL"); ok {
		if e.parallel, err = strconv.Atoi(s); err != nil || e.parallel < 1 {
			return nil, fmt.Errorf("OPSTRUSS_PARALLEL is %q, not a number above 0", s)
		}
	}
	// A run started by a program of another run is nested in it.
	if run, ok := opts.LookupEnv("OPSTRUSS_RUN"); ok && run != "" {
		if s, _ := opts.LookupEnv("OPSTRUSS_RUN_PARENTS"); s != "" {
			e.parents = strings.Split(s, ",")
		}
		e.parents = append(e.parents, run)
		if len(e.parents) > MaxDepth {
			return nil, fmt.Errorf("runs nest at most %d deep, through %s", MaxDepth, strings.Join(e.parents, ", "))
		}
	}
	if e.store, err = state.Open(state.NewLocal(filepath.Join(e.root, "state")), p.StackName, p.Name); err != nil {
		return nil, err
	}
	if e.host, err = state.HostID(); err != nil {
		return nil, err
	}
	return e, nil
}

// start begins a run: the record of the run, with the runs that a crash
// left running marked first, the holder of its locks, and its directory
// of temporary files.
func (e *engine) start(command, task string) error {
	p, opts := e.p, e.opts
	interrupted, err := record.Interrupted(e.root, p.StackName)
	if err != nil {
		return err
	}
	rec := &record.Record{Project: p.Name, Stack: p.StackName, Command: command, Task: task, Args: opts.Units,
		Host: e.host, PID: os.Getpid(), Started: e.started.Format(time.RFC3339)}
	if len(e.parents) > 0 {
		rec.Parent = e.parents[len(e.parents)-1]
	}
	rec.Hostname, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		rec.User = u.Username
	}
	if e.rec, err = record.Create(e.root, state.NewRunID, rec, e.sink()); err != nil {
		return err
	}
	e.runID = e.rec.ID()
	for _, run := range interrupted {
		e.warn("", "run %s ended without finishing its record; it is marked cancelled, as interrupted", run)
	}
	e.cleanTemp()
	e.holder = state.NewHolder(e.runID, e.host)
	e.run = &plugin.Run{Project: p.Name, Stack: p.StackName, ID: e.runID, Parents: e.parents,
		Temp: e.tempOf(e.runID), Artifacts: filepath.Join(e.root, "runs", p.StackName, e.runID, "artifacts"), Environ: opts.Environ}
	return e.run.Open()
}

// sink hands events to the caller; debug events only with OPSTRUSS_LOG=debug.
func (e *engine) sink() func(Event) {
	return func(ev Event) {
		if e.opts.Emit != nil && (ev.Level != "debug" || e.opts.Debug) {
			e.opts.Emit(ev)
		}
	}
}

// debug reports a diagnostic of the engine, which the caller sees only
// with OPSTRUSS_LOG=debug.
func (e *engine) debug(addr, format string, args ...any) {
	if e.opts.Debug {
		e.emit(Event{Type: "diagnostic", Level: "debug", Address: addr, Message: fmt.Sprintf(format, args...)})
	}
}

// close ends the run: the last event, of type run, then the record, then
// the retention of old runs, and the run's temporary files go.
func (e *engine) close(outcome string, outputs map[string]any) {
	if err := e.run.Close(); err != nil {
		e.warn("", "removing the run's temporary files: %v", err)
	}
	code := e.exit
	e.emit(Event{Type: "run", Level: "info", Task: e.rec.Task(), Message: outcome, Outcome: outcome, ExitCode: &code, Outputs: outputs})
	e.mu.Lock()
	touched := e.touched
	e.mu.Unlock()
	err := e.rec.Finish(func(r *record.Record) {
		r.Outcome, r.ExitCode, r.Outputs = outcome, code, outputs
		if len(touched) > 0 {
			r.Units = touched
		}
	})
	if err == nil {
		err = record.Prune(e.root, e.p.StackName)
	}
	if err != nil && e.opts.Emit != nil {
		e.opts.Emit(Event{Type: "diagnostic", Level: "warn", Message: "keeping the record of the run: " + err.Error()})
	}
}

// cleanTemp removes the temporary directories of runs that no longer run
// and whose programs are all gone: a crash leaves them behind.
func (e *engine) cleanTemp() {
	ents, err := os.ReadDir(filepath.Join(e.root, "tmp"))
	if err != nil {
		return
	}
	for _, ent := range ents {
		run := ent.Name()
		// A run makes its record before its temporary directory, so a run
		// that is starting is alive by its record before its programs hold
		// the directory's lock.
		if run == e.runID || record.Alive(e.root, run) {
			continue
		}
		ps, held, err := plugin.Marker(run, e.tempOf(run)).Alive()
		if err == nil && len(ps) == 0 && !held {
			_ = os.RemoveAll(e.tempOf(run))
		}
	}
}

// touch notes what the run did to a unit, for its record.
func (e *engine) touch(addr string, u *record.Unit) {
	e.mu.Lock()
	e.touched[addr] = u
	e.mu.Unlock()
}

func (e *engine) tempOf(run string) string { return filepath.Join(e.root, "tmp", run) }

func (e *engine) emit(ev Event) {
	if e.rec != nil {
		e.rec.Emit(ev)
	}
}

func (e *engine) say(typ, addr, format string, args ...any) {
	e.emit(Event{Type: typ, Level: "info", Address: addr, Message: fmt.Sprintf(format, args...)})
}

// sayTask reports on a task, or on one of its steps.
func (e *engine) sayTask(task, step, format string, args ...any) {
	e.emit(Event{Type: map[bool]string{true: "step", false: "task"}[step != ""], Level: "info", Task: task, Step: step,
		Message: fmt.Sprintf(format, args...)})
}

func (e *engine) warn(addr, format string, args ...any) {
	e.emit(Event{Type: "diagnostic", Level: "warn", Address: addr, Message: fmt.Sprintf(format, args...)})
}

// fail reports err for addr and makes the run exit with 1.
func (e *engine) fail(addr string, err error) {
	e.emit(Event{Type: "unit", Level: "error", Address: addr, Message: err.Error()})
	e.mu.Lock()
	e.exit = 1
	e.mu.Unlock()
}

// stopAtPlan makes the run exit with 2, unless something failed.
func (e *engine) stopAtPlan() {
	e.mu.Lock()
	if e.exit == 0 {
		e.exit = 2
	}
	e.mu.Unlock()
}

func (e *engine) result() *Result { return &Result{Run: e.runID, ExitCode: e.exit} }

// hostname is the name of this machine, read once.
var hostname = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	return h
})

// facts are what opstruss.* holds for the units of this run.
func (e *engine) facts(inst *instance) map[string]any {
	f := map[string]any{"project": e.p.Name, "os": runtime.GOOS, "arch": runtime.GOARCH, "host": hostname(),
		"run": e.runID, "time": e.started}
	if inst != nil {
		f["dir"], f["address"] = inst.dir, inst.address
	}
	return f
}

// lineWriter turns the output of a program into log events, one per line.
type lineWriter struct {
	e      *engine
	addr   string
	task   string
	step   string
	stream string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) line(s string) {
	w.e.emit(Event{Type: "log", Level: "info", Address: w.addr, Task: w.task, Step: w.step, Stream: w.stream, Message: strings.ToValidUTF8(s, "\uFFFD")})
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}

// lock takes the lock of addr, waiting while another run holds it; a lock
// that a run this one is nested in holds fails at once, since that run
// waits for this one.
func (e *engine) lock(ctx context.Context, addr string) (string, error) {
	var last string
	for delay := 20 * time.Millisecond; ; delay = min(2*delay, time.Second) {
		token, err := e.store.Lock(addr, e.holder)
		var le *state.LockedError
		if !errors.As(err, &le) {
			return token, err
		}
		// The holder writes who it is just after it takes the lock.
		if h := le.Holder; h.Run != "" {
			for _, p := range e.parents {
				if p == h.Run {
					return "", fmt.Errorf("the lock is held by run %s, which this run is nested in", h.Run)
				}
			}
			if h.Run != last {
				e.say("unit", addr, "waiting for the lock held by %s", h)
				last = h.Run
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for the lock: %w", context.Cause(ctx))
		case <-time.After(delay):
		}
	}
}

// waitForPrograms waits while programs of the interrupted attempt a still
// live on this host, since the lock of a killed OpsTruss is free while its
// programs may still change the unit.
func (e *engine) waitForPrograms(ctx context.Context, addr string, a *state.Attempt) error {
	if a.Holder.Host != e.host {
		return nil
	}
	return plugin.Marker(a.Run, e.tempOf(a.Run)).WaitGone(ctx, func(ps []proc.Process, held bool) {
		what := "programs"
		if len(ps) > 0 {
			what = fmt.Sprint(ps)
		}
		e.say("unit", addr, "waiting for %s of interrupted run %s", what, a.Run)
	})
}
