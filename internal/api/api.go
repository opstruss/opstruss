// Package api is the engine API: what the command line and the socket of
// a daemon ask of the engine, and later MCP. Requests and responses are
// JSON, so a call can cross a socket as it is; the events of a run and the
// questions that wait for an answer come back through callbacks.
package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// Protocol is the version of the API, which a connection opens with in
// initialize; a client that meets another one stops.
const Protocol = 1

// Request says which project and stack a call is about, with what the
// caller brings: its directory, environment, and inputs, which a run
// forwarded to a daemon uses as the caller's.
type Request struct {
	Locked bool `json:"locked,omitempty"`

	Dir     string            `json:"dir"`
	Cwd     string            `json:"cwd,omitempty"`
	Stack   string            `json:"stack,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Environ []string          `json:"environ,omitempty"`
	Units   []string          `json:"units,omitempty"`
	Yes     bool              `json:"yes,omitempty"`
	Color   bool              `json:"color,omitempty"` // the caller shows the output of the run in color
	Force   bool              `json:"force,omitempty"` // up: although unchanged; destroy: although pass values differ; cancel: kill at once
	NoDeps  bool              `json:"no_deps,omitempty"`
	Follow  bool              `json:"follow,omitempty"` // logs: go on with what is written
	Limit   int               `json:"limit,omitempty"`  // runs: only that many of the newest; 0 reads all
	Task    string            `json:"task,omitempty"`   // run: the task
	With    map[string]any    `json:"with,omitempty"`   // run: its inputs
	Flags   map[string]any    `json:"flags,omitempty"`  // run: inputs as flags wrote them, read by each task reached, over With
	Event   map[string]any    `json:"event,omitempty"`  // run: an event that the caller wrote for a trigger of the task, as --event
	Run     string            `json:"run,omitempty"`    // events: the run to read; cancel and rerun: the run
	Debug   bool              `json:"debug,omitempty"`
	// Detach leaves the runs after this one to the daemon, as -d does,
	// rather than waiting for them.
	Detach bool `json:"detach,omitempty"`
	// ShowSensitive gives outputs their sensitive values.
	ShowSensitive bool `json:"show_sensitive,omitempty"`
}

// Result is how a run ended; Record, only in the process that ran it, is
// its record as it finished, also of a run whose directory went.
type Result struct {
	Run      string         `json:"run"`
	ExitCode int            `json:"exit_code"`
	Record   *record.Record `json:"-"`
	// Inputs, only in the process that ran it, are the --input of a rerun,
	// which it took from the run it ran again.
	Inputs map[string]string `json:"-"`
	// Followers, only in the process that ran it, are the runs that it
	// made through after:.
	Followers []*engine.Follower `json:"-"`
}

// Handlers are the callbacks of a call: the events of its run, and the
// questions that wait for an answer at a terminal. A nil Approve leaves a
// change that needs approval at plan.
type Handlers struct {
	Event   func(record.Event)
	Approve func(context.Context, engine.Question) (bool, error)
	Ask     func(context.Context, string, *model.Input) (string, error)
	// Terminal is the terminal that shows the run as it goes, in this
	// process only: a call through the socket has none.
	Terminal *engine.Terminal
}

// API answers calls in this process. Supervisor keeps the services that
// its runs start; without one, a service cannot start.
type API struct {
	h          Handlers
	Supervisor engine.Supervisor
	Daemon     bool // this process is the daemon of the stack
	// Install, in the daemon, installs the triggers of the stack again for
	// an up of r with inputs.
	Install func(r Request, inputs map[string]string) error
	// Queue, in the daemon, holds a place in the queue of task for a run
	// after another, as its triggers do, until end; an error, as of a full
	// queue, leaves that run out.
	Queue func(task string) (end func(), err error)
	// Later, in the daemon, takes the runs after a run whose caller does
	// not wait for them, as -d leaves them.
	Later func(fs []*engine.Follower)
	// Log, in the daemon, also writes what the runs after a run say about
	// their triggers to its log, as it does for the runs of its triggers.
	Log func(level, task, msg string)
}

// New returns the API of this process with h for the callbacks of calls.
func New(h Handlers) *API { return &API{h: h} }

func (a *API) options(r Request) engine.Options {
	env := map[string]string{}
	for _, kv := range r.Environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	o := engine.Options{Locked: r.Locked, Dir: r.Dir, Cwd: r.Cwd, Stack: r.Stack, Inputs: r.Inputs, Flags: r.Flags, Environ: r.Environ,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Units:     r.Units, Yes: r.Yes, Color: r.Color, Force: r.Force, NoDeps: r.NoDeps, Follow: r.Follow, Limit: r.Limit, Debug: r.Debug, ShowSensitive: r.ShowSensitive,
		Emit: a.h.Event, Approve: a.h.Approve, Ask: a.h.Ask, Terminal: a.h.Terminal, Supervisor: a.Supervisor, Daemon: a.Daemon}
	if a.Install != nil {
		o.Install = func(inputs map[string]string) error { return a.Install(r, inputs) }
	}
	return o
}

func result(res *engine.Result, err error) (*Result, error) {
	if err != nil {
		return nil, err
	}
	return &Result{Run: res.Run, ExitCode: res.ExitCode, Record: res.Record, Inputs: res.Inputs, Followers: res.Followers}, nil
}

// Call runs the method of Methods named method, then the runs it makes
// through after:, each with its own chain, in this process, which the
// command waits for and whose codes join its own: 1 when one failed, else
// 2 when one stopped at plan.
func (a *API) Call(ctx context.Context, method string, r Request) (*Result, error) {
	res, err := a.call(ctx, method, r)
	if err != nil || res == nil {
		return res, err
	}
	// The daemon goes on with them when its caller does not wait: -d, or
	// a caller that stopped the run.
	if (r.Detach || ctx.Err() != nil) && a.Later != nil {
		a.Later(res.Followers)
		return res, nil
	}
	res.ExitCode = a.Follow(ctx, r, res.Followers, res.ExitCode, 0)
	return res, nil
}

// Follow runs fs, the runs after one whose command ends with code, which
// is depth deep in a chain of after, one at a time and each with its own
// chain, and returns the code of the command.
func (a *API) Follow(ctx context.Context, r Request, fs []*engine.Follower, code, depth int) int {
	say := func(level, task, msg string) {
		if a.Log != nil {
			a.Log(level, task, msg)
		}
		if a.h.Event != nil {
			a.h.Event(record.Event{Version: record.EventVersion, Time: record.Stamp(time.Now()), Type: "trigger", Level: level, Task: task, Message: msg})
		}
	}
	for _, f := range fs {
		if ctx.Err() != nil {
			return code
		}
		if depth+1 > engine.MaxDepth {
			say("warn", f.Trigger.Task, fmt.Sprintf("%s: run %v is %d deep in a chain of after, so it makes no run", f.Trigger, f.Fields["run"], depth))
			continue
		}
		opts := a.options(Request{Locked: r.Locked, Dir: r.Dir, Cwd: r.Cwd, Stack: f.Stack, Inputs: r.Inputs, Environ: r.Environ, Color: r.Color, Debug: r.Debug})
		opts.Event = &engine.TriggerEvent{Trigger: "after", Time: time.Now().UTC(), Fields: f.Fields}
		var end func()
		if a.Queue != nil {
			var err error
			if end, err = a.Queue(f.Trigger.Task); err != nil {
				say("warn", f.Trigger.Task, fmt.Sprintf("%s: made no run: %v", f.Trigger, err))
				continue
			}
		}
		res, err := engine.Fire(ctx, opts, f.Trigger)
		if end != nil {
			end()
		}
		switch {
		case err != nil:
			say("error", f.Trigger.Task, fmt.Sprintf("%s: %v", f.Trigger, err))
			code = worse(code, 1)
		case res != nil:
			// A nil res is a run that if: of the trigger left out.
			code = a.Follow(ctx, r, res.Followers, worse(code, res.ExitCode), depth+1)
		}
	}
	return code
}

// worse is the code of a command whose runs end with a and b: a signal
// over a failure, a failure over a stop at plan, which is over success.
func worse(a, b int) int {
	rank := func(c int) int {
		switch {
		case c >= 128:
			return 3
		case c == 1:
			return 2
		case c == 2:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func (a *API) call(ctx context.Context, method string, r Request) (*Result, error) {
	switch method {
	case "plan":
		return a.Plan(ctx, r)
	case "up":
		return a.Up(ctx, r)
	case "destroy":
		return a.Destroy(ctx, r)
	case "status":
		return a.StatusUnits(ctx, r)
	case "run":
		return a.Run(ctx, r)
	case "unlock":
		return a.Unlock(ctx, r)
	case "rerun":
		return a.Rerun(ctx, r)
	}
	return nil, fmt.Errorf("the engine API has no method %q", method)
}

// Plan previews the units of the request.
func (a *API) Plan(ctx context.Context, r Request) (*Result, error) {
	return result(engine.PlanContext(ctx, a.options(r)))
}

// Up brings the units of the request up.
func (a *API) Up(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Up(ctx, a.options(r)))
}

// Destroy destroys the units of the request.
func (a *API) Destroy(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Destroy(ctx, a.options(r)))
}

// Status runs status of the units of the request.
func (a *API) StatusUnits(ctx context.Context, r Request) (*Result, error) {
	return result(engine.StatusUnits(ctx, a.options(r)))
}

// Exec runs argv, a program of the operator, for the one unit of the
// request, attached to the terminal tty; it stays in this process, since
// the terminal does not cross a socket.
func (a *API) Exec(ctx context.Context, r Request, argv []string, tty engine.Stdio) (*Result, error) {
	return result(engine.Exec(ctx, a.options(r), argv, tty))
}

// Logs shows the output of the units of the request; it runs in this
// process also while a daemon runs, since it only reads.
func (a *API) Logs(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Logs(ctx, a.options(r)))
}

// Unlock forgets what is left of the service of the one unit of the
// request that cannot be proven its own.
func (a *API) Unlock(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Unlock(ctx, a.options(r)))
}

// Run runs the task of the request with its inputs, or, with an event,
// as its trigger runs it for that event; a trigger that leaves the event
// out makes no run, and the result names none.
func (a *API) Run(ctx context.Context, r Request) (*Result, error) {
	if r.Event == nil {
		return result(engine.Task(ctx, a.options(r), r.Task, r.With))
	}
	res, err := engine.Simulate(ctx, a.options(r), r.Task, r.With, r.Event)
	if err == nil && res == nil {
		return &Result{}, nil
	}
	return result(res, err)
}

// Rerun runs the run of the request again, with the inputs and the event it
// had, through the project as it is now; Yes and Force are the caller's.
func (a *API) Rerun(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Rerun(ctx, a.options(r), r.Run))
}

// Outputs returns the outputs of the project.
func (a *API) Outputs(_ context.Context, r Request) (*engine.OutputList, error) {
	return engine.Outputs(a.options(r))
}

// Doctor checks what a run of the stack needs on this machine.
func (a *API) Doctor(_ context.Context, r Request) (*engine.Report, error) {
	return engine.Doctor(a.options(r))
}

// Status returns the tasks and units of the stack.
func (a *API) Status(_ context.Context, r Request) (*engine.Status, error) {
	return engine.List(a.options(r))
}

// Runs returns the records of the stack's runs, newest first.
func (a *API) Runs(_ context.Context, r Request) (*engine.RunList, error) {
	return engine.Runs(a.options(r))
}

// Cancel asks the run of the request to cancel, with Force to kill its
// programs at once, and waits until it ends. It stays in this process,
// as reads do: the run, wherever it runs, reads the request itself.
func (a *API) Cancel(ctx context.Context, r Request) (*record.Record, error) {
	return engine.Cancel(ctx, a.options(r), r.Run, r.Force)
}

// Events hands the stored events of the run of the request to the Event
// callback, in order, and with Follow what it writes until it ends.
func (a *API) Events(ctx context.Context, r Request) error {
	return engine.Events(ctx, a.options(r), r.Run, a.h.Event)
}
