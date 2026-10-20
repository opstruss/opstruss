// Package engine runs a stack: the plan, up, and down of its units in
// graph order, each under the lock of its address, with what each unit did
// in its record.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
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
}

// Event is one thing that happened in a run. The last event of a task's
// run carries its outcome and outputs.
type Event struct {
	Type    string         `json:"type"`
	Level   string         `json:"level"`
	Address string         `json:"address,omitempty"`
	Task    string         `json:"task,omitempty"`
	Stream  string         `json:"stream,omitempty"`
	Message string         `json:"message"`
	Outcome string         `json:"outcome,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
}

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

	rootTask string
	needed   map[string]*taskOnce               // tasks of needs, which run once per run
	holders  map[string]context.CancelCauseFunc // concurrency locks this process holds

	insts  []*instance // in graph order
	byUnit map[string][]*instance

	moveFrom   map[string]*state.Record // the record a target of moved: starts from
	moveFailed map[string]error         // targets whose move cannot happen
	moving     map[string]bool          // addresses that moved: moves away from

	mu   sync.Mutex
	exit int
}

func open(opts Options) (*engine, error) {
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
		runID: state.NewRunID(), started: time.Now().UTC(), parallel: DefaultParallel, byUnit: map[string][]*instance{},
		read: map[string]string{}, needed: map[string]*taskOnce{}, holders: map[string]context.CancelCauseFunc{}}
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
	e.holder = state.NewHolder(e.runID, e.host)
	e.run = &plugin.Run{Project: p.Name, Stack: p.StackName, ID: e.runID, Parents: e.parents,
		Temp: e.tempOf(e.runID), Artifacts: filepath.Join(e.root, "runs", p.StackName, e.runID, "artifacts"), Environ: opts.Environ}
	if err := e.run.Open(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *engine) close() {
	if err := e.run.Close(); err != nil {
		e.warn("", "removing the run's temporary files: %v", err)
	}
}

func (e *engine) tempOf(run string) string { return filepath.Join(e.root, "tmp", run) }

func (e *engine) emit(ev Event) {
	if e.opts.Emit == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.Emit(ev)
}

func (e *engine) say(typ, addr, format string, args ...any) {
	e.emit(Event{Type: typ, Level: "info", Address: addr, Message: fmt.Sprintf(format, args...)})
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

// facts are what opstruss.* holds for the units of this run.
func (e *engine) facts(inst *instance) map[string]any {
	host, _ := os.Hostname()
	f := map[string]any{"project": e.p.Name, "os": runtime.GOOS, "arch": runtime.GOARCH, "host": host,
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
	w.e.emit(Event{Type: "log", Level: "info", Address: w.addr, Task: w.task, Stream: w.stream, Message: strings.ToValidUTF8(s, "�")})
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
