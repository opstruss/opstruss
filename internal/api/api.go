// Package api is the engine API: what the command line asks of the
// engine, and later MCP and the socket of the daemon. Requests and
// responses are JSON, so a call can cross a socket as it is; the events of
// a run and the questions that wait for an answer come back through
// callbacks.
package api

import (
	"context"
	"strings"

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
	Dir     string            `json:"dir"`
	Cwd     string            `json:"cwd,omitempty"`
	Stack   string            `json:"stack,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Environ []string          `json:"environ,omitempty"`
	Units   []string          `json:"units,omitempty"`
	Yes     bool              `json:"yes,omitempty"`
	Force   bool              `json:"force,omitempty"`
	NoDeps  bool              `json:"no_deps,omitempty"`
	Task    string            `json:"task,omitempty"` // run: the task
	With    map[string]any    `json:"with,omitempty"` // run: its inputs
	Run     string            `json:"run,omitempty"`  // events: the run to read
	Debug   bool              `json:"debug,omitempty"`
	// ShowSensitive gives outputs their sensitive values.
	ShowSensitive bool `json:"show_sensitive,omitempty"`
}

// Result is how a run ended.
type Result struct {
	Run      string `json:"run"`
	ExitCode int    `json:"exit_code"`
}

// Handlers are the callbacks of a call: the events of its run, and the
// questions that wait for an answer at a terminal. A nil Approve leaves a
// change that needs approval at plan.
type Handlers struct {
	Event   func(record.Event)
	Approve func(context.Context, engine.Question) (bool, error)
	Ask     func(context.Context, string, *model.Input) (string, error)
}

// API answers calls in this process.
type API struct{ h Handlers }

// New returns the API of this process with h for the callbacks of calls.
func New(h Handlers) *API { return &API{h: h} }

func (a *API) options(r Request) engine.Options {
	env := map[string]string{}
	for _, kv := range r.Environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return engine.Options{Dir: r.Dir, Cwd: r.Cwd, Stack: r.Stack, Inputs: r.Inputs, Environ: r.Environ,
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Units:     r.Units, Yes: r.Yes, Force: r.Force, NoDeps: r.NoDeps, Debug: r.Debug, ShowSensitive: r.ShowSensitive,
		Emit: a.h.Event, Approve: a.h.Approve, Ask: a.h.Ask}
}

func result(res *engine.Result, err error) (*Result, error) {
	if err != nil {
		return nil, err
	}
	return &Result{Run: res.Run, ExitCode: res.ExitCode}, nil
}

// Plan previews the units of the request.
func (a *API) Plan(_ context.Context, r Request) (*Result, error) {
	return result(engine.Plan(a.options(r)))
}

// Up brings the units of the request up.
func (a *API) Up(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Up(ctx, a.options(r)))
}

// Down takes the units of the request down.
func (a *API) Down(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Down(ctx, a.options(r)))
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

// Run runs the task of the request with its inputs.
func (a *API) Run(ctx context.Context, r Request) (*Result, error) {
	return result(engine.Task(ctx, a.options(r), r.Task, r.With))
}

// Outputs returns the outputs of the project.
func (a *API) Outputs(_ context.Context, r Request) (map[string]any, error) {
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
func (a *API) Runs(_ context.Context, r Request) ([]*record.Record, error) {
	return engine.Runs(a.options(r))
}

// Events hands the stored events of the run of the request to the Event
// callback, in order.
func (a *API) Events(_ context.Context, r Request) error {
	return engine.Events(a.options(r), r.Run, a.h.Event)
}
