// Package engine runs a stack: the plan, up, and down of its units in
// graph order, each under the lock of its address, with what each unit did
// in its record.
package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// Options says what a run does.
type Options struct {
	Locked bool

	Dir    string            // the project directory
	Cwd    string            // where OpsTruss started, before -C: opstruss.cwd
	Stack  string            // empty chooses the only stack
	Inputs map[string]string // --input
	// Flags are inputs of a task as a command line writes them: a text,
	// the texts of a flag given more than once, or a boolean. Each task
	// that the call reaches reads them by the types of its own inputs,
	// over the inputs of the call.
	Flags     map[string]any
	LookupEnv func(string) (string, bool)
	Environ   []string // the operator's environment
	Units     []string // names or patterns; none means every unit
	Yes       bool     // approve the changes of this run
	Force     bool     // up: run the units named although unchanged; down: although pass values differ
	NoDeps    bool     // leave out the dependencies of up and the dependents of down
	Follow    bool     // logs: go on with what services and plugins write

	ShowSensitive bool // outputs: give sensitive values as they are
	Emit          func(Event)
	// Approve asks whoever is at a terminal to approve one change, one
	// question at a time; without it, a change that needs approval stops
	// at plan. Ask asks for a required input that has no value.
	Approve func(context.Context, Question) (bool, error)
	Ask     func(context.Context, string, *model.Input) (string, error)
	Debug   bool // OPSTRUSS_LOG=debug: diagnostics of the engine as debug events
	// Supervisor keeps the services of the stack; without one, a service
	// cannot start.
	Supervisor Supervisor
	// Model is the project as a daemon read it when it installed its
	// triggers, for the runs they make; without it, the files are read.
	Model *model.Project
	// Event is what made the run; without it, the command line did.
	Event *TriggerEvent
	// Started hears the id of a run made by Fire once its record is there.
	Started func(run string)
	// RerunOf is the run that this one runs again, as runs rerun does.
	RerunOf string
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
	Outcome  string
	Outputs  map[string]any    // of a task, with sensitive values as secret.Mask
	Record   *record.Record    // as the run finished it, also when its directory went
	Inputs   map[string]string // --input of the run, which a rerun takes from the run it runs again
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
	releaseSources func()

	ctx      context.Context // the run's, for the programs of secrets, which an expression starts without one of its own
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
	ran      map[string]*record.Task // the tasks of the project that ran inside the run, through task:, needs, or a unit

	rootTask string
	needed   map[string]*taskOnce // tasks of needs, which run once per run
	event    map[string]any       // event: what made the run
	trigger  bool                 // reads the expressions of a trigger, which outside a run read secrets from sops and env
	outcome  string               // how the run ended, once it closed
	outputs  map[string]any       // and what it gave, as shown

	insts  []*instance            // in graph order
	byUnit map[string][]*instance // by the path of the unit
	top    *frame                 // the project, where its units are
	frames map[string]*frameSet   // instances of components, by the path of the unit that uses each

	moveFrom   map[string]*state.Record // the record a target of moved: starts from
	moveFailed map[string]error         // targets whose move cannot happen
	moving     map[string]bool          // addresses that moved: moves away from
	pairs      []movePair               // the records that moved: moves, in order
	have       map[string]bool          // the addresses with records, as the moves of the pass leave them
	collect    bool                     // the pass previews the moves of the instances of components it finds
	stale      []string                 // records of instances of components that the pass did not find

	mu       sync.Mutex
	exit     int
	promptMu sync.Mutex
	inPass   bool              // a pass is expanding and planning units
	kv       *kvMemory         // what kv() reads in this pass
	kvLocals bool              // a local reads kv()
	hashes   map[string]string // of plugin directories, read once a run

	redact     *secret.Redactor // hides the sensitive values the run knows in what it writes out
	shortPass  map[string]bool  // the variables passed whose value is too short to hide, warned of; under mu
	inputMarks map[string]any   // the sensitive inputs of the project
	localMarks map[string]any   // the locals that read them, or a secret
	tooShort   []string         // sensitive values too short to hide, by name, for a warning once the run starts

	secrets   map[string]any // secrets.<name>, each resolved when an expression of the run first reads it
	sopsMu    sync.Mutex
	sopsFiles map[string]any // the sops files of secrets, by path, each decrypted once a run

	keyOnce sync.Once
	key     []byte // the data key of the stack, once a value needed it
	keyErr  error
	writes  bool              // the run writes state, so it wraps the data key again for encryption_keys
	newIDs  map[string]string // the ids of records that up makes, by address

	release   func()         // ends the context that control made, once the run closed
	cancelReq *record.Cancel // the first request to cancel the run; under mu
	killReq   bool           // a request with force came; under mu
}

// open reads the project and the state of its stack, and starts the
// record of a run of command, or of the task with that name.
//
// The run's context, e.ctx, also ends on a request in its cancel.jsonl, so
// callers go on with it, not with ctx.
func open(ctx context.Context, opts Options, command, task string) (*engine, error) {
	e, err := loadContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	e.ctx = ctx
	if err := e.start(command, task, nil, nil); err != nil {
		e.releaseSources()
		return nil, err
	}
	return e, nil
}

// load reads the project and opens the state of its stack, for a run or
// for a command that only reads.
func load(opts Options) (*engine, error) { return loadContext(context.Background(), opts) }

func loadContext(ctx context.Context, opts Options) (*engine, error) {
	p := opts.Model
	if p != nil {
		session, err := p.Sources.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		copy := *p
		copy.Sources = session
		p = &copy
	}
	if p == nil {
		var err error
		if p, err = model.LoadWith(ctx, opts.Dir, opts.Stack, sources.Options{Locked: opts.Locked}); err != nil {
			return nil, err
		}
	}
	releaseSources := p.Sources.Retain()
	p.Close()
	loaded := false
	defer func() {
		if !loaded {
			releaseSources()
		}
	}()
	// A sensitive input is known before it is checked, so the error of the
	// check hides it.
	red := secret.NewRedactor()
	var short []string
	values, err := p.ResolveInputs(opts.Inputs, opts.LookupEnv, func(name string, v any) {
		if red.Add(v) {
			short = append(short, "input "+name)
		}
	})
	if err != nil {
		return nil, red.Err(err)
	}
	inputs, inputMarks := map[string]any{}, map[string]any{}
	for k, v := range values {
		inputs[k] = v.Value
		if p.Inputs[k].Sensitive {
			red.Add(v.Value)
			inputMarks[k] = true
		}
	}
	e := &engine{releaseSources: releaseSources, opts: opts, p: p, inputs: inputs, root: filepath.Join(p.Dir, ".opstruss"),
		redact: red, inputMarks: inputMarks, tooShort: short,
		started: time.Now().UTC(), parallel: DefaultParallel, byUnit: map[string][]*instance{},
		read: map[string]string{}, needed: map[string]*taskOnce{},
		touched: map[string]*record.Unit{}, ran: map[string]*record.Task{}, hashes: map[string]string{}, frames: map[string]*frameSet{},
		sopsFiles: map[string]any{}}
	e.event = eventOf(opts.Event, e.started)
	e.secrets = e.secretValues()
	e.localMarks = p.LocalMarks(e.projectMarks(inputMarks), nil)
	if e.locals, err = e.evalLocals(nil); err != nil {
		return nil, red.Err(err)
	}
	e.top = &frame{in: p, address: p.StackName}
	for _, v := range p.Locals {
		if use, err := expr.AnalyzeValue(v); err == nil && slices.Contains(use.Funcs, "kv") {
			e.kvLocals = true
		}
	}
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
	e.store.Key = e.dataKey
	e.store.Known = func(_, _ string, v any) bool { return e.redact.Contains(v) }
	e.store.Opened = func(v any) { e.redact.Add(v) }
	if e.host, err = state.HostID(); err != nil {
		return nil, err
	}
	loaded = true
	return e, nil
}

// start begins a run: the record of the run, the holder of its locks, and
// its directory of temporary files. with are the inputs that its caller
// gave the task, and tr the trigger whose event made it, which runs rerun
// gives again.
func (e *engine) start(command, task string, with map[string]any, tr *Trigger) error {
	p, opts := e.p, e.opts
	rec := &record.Record{Project: p.Name, Stack: p.StackName, Command: command, Task: task, Args: opts.Units,
		Trigger: e.event["trigger"].(string), Host: e.host, PID: os.Getpid(), Started: e.started.Format(time.RFC3339Nano),
		NoDeps: opts.NoDeps, RerunOf: opts.RerunOf}
	var rp *replay
	rec.Replay, rp = e.replayOf(command, with, tr)
	if ev := opts.Event; ev != nil && ev.Simulated {
		// The event is the caller's; the run comes from the command line.
		rec.Trigger, rec.Simulated = "cli", ev.Trigger
	}
	if len(e.parents) > 0 {
		rec.Parent = e.parents[len(e.parents)-1]
	}
	rec.Hostname, rec.User = hostname(), username()
	var err error
	if e.rec, err = record.Create(e.root, state.NewRunID, rec, e.sink()); err != nil {
		return err
	}
	e.runID = e.rec.ID()
	e.control()
	// The run is in its record now, which is what a webhook answers after;
	// the rest of the start comes after that answer.
	if opts.Started != nil {
		opts.Started(e.runID)
	}
	if opts.RerunOf != "" {
		e.say("diagnostic", "", "runs run %s again", opts.RerunOf)
	}
	if rp != nil {
		e.keepReplay(rp)
	}
	for _, name := range e.tooShort {
		e.warnShort(name)
	}
	e.cleanTemp()
	e.holder = state.NewHolder(e.runID, e.host)
	run := &plugin.Run{SourceFiles: e.p.Sources.Files, RetainSources: e.p.Sources.Retain, PluginBins: e.p.PluginBinaries(), Project: p.Name, Stack: p.StackName, ID: e.runID, Parents: e.parents,
		Temp: e.tempOf(e.runID), Artifacts: filepath.Join(e.root, "runs", p.StackName, e.runID, "artifacts"), Environ: opts.Environ,
		Passed: e.passed}
	if err := run.Open(); err != nil {
		e.abort(err)
		return err
	}
	e.run = run
	return nil
}

// abort ends a run whose start failed after its record was made: failed,
// with err among its events. The caller reports err itself, so the events
// go to the record alone.
func (e *engine) abort(err error) {
	e.opts.Emit = nil
	e.fail("", err)
	e.close(failure, nil, nil)
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

// close ends the run: its record, then the runs that a crash left running
// marked and old runs dropped, then the last event, of type run, whose
// exit code says whether the record was written; the run's temporary
// files go first. The outputs that marks
// marks are written out as secret.Mask.
func (e *engine) close(outcome string, outputs, marks map[string]any) {
	if e.run != nil {
		if err := e.run.Close(); err != nil {
			e.warn("", "removing the run's temporary files: %v", err)
		}
	}
	outputs = e.shown(outputs, marks)
	reason := e.cancelledBy()
	if reason != "" && outcome != success {
		outcome = cancelled
	}
	e.outcome, e.outputs = outcome, outputs
	e.mu.Lock()
	touched, ran, code := e.touched, e.ran, e.exit
	e.mu.Unlock()
	err := e.rec.Finish(func(r *record.Record) {
		r.Outcome, r.ExitCode, r.Outputs = outcome, code, outputs
		if outcome == cancelled && reason != "" {
			r.Reason = reason
		}
		if len(touched) > 0 {
			r.Units = touched
		}
		if len(ran) > 0 {
			r.Tasks = ran
		}
	})
	if err != nil {
		// What the run did stands, and is not done again; only its record
		// is missing, until the next run marks it from its events.
		e.emit(Event{Type: "diagnostic", Level: "error",
			Message: fmt.Sprintf("the record of run %s was not written: %v; the next run of the stack marks it from its events", e.runID, err)})
		if code == 0 || code == 2 {
			code = 1
		}
	} else {
		marked, err := record.Prune(e.root, e.p.StackName)
		for _, r := range marked {
			if r.Reason == "interrupted" {
				e.warn("", "run %s ended without finishing its record; it is marked cancelled, as interrupted", r.Run)
			} else {
				e.warn("", "run %s ended without writing its record; it is marked %s, as its last event says", r.Run, r.Outcome)
			}
		}
		if err != nil {
			e.warn("", "%v", err)
		}
	}
	e.mu.Lock()
	e.exit = code
	e.mu.Unlock()
	e.emit(Event{Type: "run", Level: "info", Task: e.rec.Task(), Message: outcome, Outcome: outcome, ExitCode: &code, Outputs: outputs})
	e.rec.Close()
	if e.release != nil {
		e.release()
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
		// A run that just finished removed its directory itself; only one
		// that a crash left needs the processes looked through.
		if _, err := os.Stat(e.tempOf(run)); err != nil {
			continue
		}
		ps, held, err := plugin.Marker(run, e.tempOf(run)).Alive()
		if err == nil && len(ps) == 0 && !held {
			_ = os.RemoveAll(e.tempOf(run))
		}
	}
}

// resetUnits forgets the instances of the units and components that the
// last pass found, for a pass that finds them again.
func (e *engine) resetUnits() {
	e.mu.Lock()
	e.insts, e.byUnit, e.frames = nil, map[string][]*instance{}, map[string]*frameSet{}
	e.mu.Unlock()
}

// ranTask notes that the task name of the project ran inside the run, for
// its record; calls of it end as one outcome, as those of for_each do.
func (e *engine) ranTask(name, outcome string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t := e.ran[name]; t == nil || rank[outcome] > rank[t.Outcome] {
		e.ran[name] = &record.Task{Outcome: outcome}
	}
}

// touch notes what the run did to a unit, for its record.
func (e *engine) touch(addr string, u *record.Unit) {
	e.mu.Lock()
	e.touched[addr] = u
	e.mu.Unlock()
}

func (e *engine) tempOf(run string) string { return filepath.Join(e.root, "tmp", run) }

// emit writes an event out as the run writes out what it says: every
// sensitive value the run knows hidden, and the shapes of well-known
// credentials (secret.Redactor.Log).
func (e *engine) emit(ev Event) {
	ev.Message = e.redact.Log(ev.Message)
	if ev.Outputs != nil {
		ev.Outputs = e.redact.LogValue(ev.Outputs).(map[string]any)
	}
	e.send(ev)
}

// send writes out an event whose values are hidden already: the one place
// where what the run says leaves it.
func (e *engine) send(ev Event) {
	if e.rec != nil {
		e.rec.Emit(ev)
	}
}

// warnShort warns that a sensitive value is too short to hide.
func (e *engine) warnShort(name string) {
	e.warn("", "%s is shorter than %d characters, so the output does not hide it", name, secret.MinLen)
}

// known makes v, a sensitive value the run was given, hidden in what the
// run writes out, with a warning when it is too short for that.
func (e *engine) known(name string, v any) {
	if e.redact.Add(v) {
		e.warnShort(name)
	}
}

// passed makes the value of a variable that pass: lets through to a
// program hidden in what the run writes out, as a secret, when its name
// says that it is one (secret.HiddenName); a value too short to hide is
// warned of once.
func (e *engine) passed(name, value string) {
	if !secret.HiddenName(name) || !e.redact.Add(value) {
		return
	}
	e.mu.Lock()
	warned := e.shortPass[name]
	if e.shortPass == nil {
		e.shortPass = map[string]bool{}
	}
	e.shortPass[name] = true
	e.mu.Unlock()
	if !warned {
		e.warnShort("variable " + name)
	}
}

// hide makes the parts of v that marks marks hidden in what the run writes
// out, as values computed from sensitive ones.
func (e *engine) hide(v, marks any) {
	switch m := marks.(type) {
	case bool:
		if m {
			e.redact.AddDerived(v)
		}
	case map[string]any:
		vm, ok := v.(map[string]any)
		if !ok {
			e.redact.AddDerived(v)
			return
		}
		for k, sub := range m {
			e.hide(vm[k], sub)
		}
	case []any:
		vl, ok := v.([]any)
		if !ok || len(vl) != len(m) {
			e.redact.AddDerived(v)
			return
		}
		for i, sub := range m {
			e.hide(vl[i], sub)
		}
	}
}

// markContent marks the outputs that hold a sensitive value the run
// knows, as GitHub Actions does for job outputs, and says which; marks
// may be nil, and the marks are returned.
func (e *engine) markContent(outputs, marks map[string]any, ev Event) map[string]any {
	for _, k := range slices.Sorted(maps.Keys(outputs)) {
		if marks[k] != nil || !e.redact.Contains(outputs[k]) {
			continue
		}
		if marks == nil {
			marks = map[string]any{}
		}
		marks[k] = true
		ev.Type, ev.Level, ev.Message = "diagnostic", "warn", fmt.Sprintf("output %s holds a sensitive value, so it is sensitive", k)
		e.emit(ev)
	}
	return marks
}

// shown is outputs as the run writes them out: the outputs that marks
// marks as secret.Mask, and the sensitive values the run knows hidden in
// the others.
func (e *engine) shown(outputs, marks map[string]any) map[string]any {
	if outputs == nil {
		return nil
	}
	out := make(map[string]any, len(outputs))
	for k, v := range outputs {
		if marks[k] != nil {
			out[k] = secret.Mask
			continue
		}
		out[k] = e.redact.Value(v)
	}
	return out
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

func (e *engine) result() *Result {
	return &Result{Run: e.runID, ExitCode: e.exit, Outcome: e.outcome, Outputs: e.outputs, Record: e.rec.Record()}
}

// username is the name of the user, read once.
var username = sync.OnceValue(func() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
})

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

// projectMarks are the marks of what the expressions in the files of the
// project read: its inputs that inputMarks marks, and every secret.
func (e *engine) projectMarks(inputMarks map[string]any) map[string]any {
	marks := map[string]any{"inputs": inputMarks}
	if len(e.secrets) > 0 {
		marks["secrets"] = true
	}
	return marks
}

// evalLocals evaluates the locals of the project, with kv() through kv; a
// sensitive one is hidden in what the run writes out once it is made.
func (e *engine) evalLocals(kv func(string) (any, error)) (map[string]any, error) {
	locals, err := e.p.EvalLocals(e.inputs, kv, e.secrets)
	if err != nil {
		return nil, err
	}
	for k := range e.localMarks {
		v := locals[k]
		if !expr.IsLater(v) {
			e.redact.AddDerived(v)
			continue
		}
		locals[k] = expr.Later(func() (any, error) {
			made, err := expr.Made(v)
			if err == nil {
				e.redact.AddDerived(made)
			}
			return made, err
		})
	}
	return locals, nil
}

// lineWriter turns the output of a program into log events, one per line,
// with what the run hides hidden; a line longer than secret.MaxLine goes in
// pieces.
type lineWriter struct {
	e      *engine
	addr   string
	task   string
	step   string
	stream string
	lines  *secret.Lines
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.lines == nil {
		w.lines = secret.NewLines(w.e.redact.Stream(), func(text string) {
			w.e.send(Event{Type: "log", Level: "info", Address: w.addr, Task: w.task, Step: w.step, Stream: w.stream, Message: text})
		})
	}
	return w.lines.Write(p)
}

func (w *lineWriter) flush() {
	if w.lines != nil {
		w.lines.Flush()
	}
}

// lock takes the lock of addr, waiting while another run holds it; a lock
// that a run this one is nested in holds fails at once, since that run
// waits for this one.
func (e *engine) lock(ctx context.Context, addr string) (string, error) {
	return e.waitLock(ctx, addr, false)
}

// briefLock takes the lock of the record of an instance of a component,
// which every run holds only for a moment, also a run this one is nested
// in, so it waits for it too.
func (e *engine) briefLock(ctx context.Context, addr string) (string, error) {
	return e.waitLock(ctx, addr, true)
}

func (e *engine) waitLock(ctx context.Context, addr string, brief bool) (string, error) {
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
				if p == h.Run && !brief {
					return "", fmt.Errorf("the lock is held by run %s, which this run is nested in", h.Run)
				}
			}
			if h.Run != last && h.Run != e.runID {
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
