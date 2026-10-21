package engine

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// progSpec is the program of a task or a step, before its expressions are
// evaluated.
type progSpec struct {
	label, task string
	step        string // the label of a step: its outputs have stdout, stderr, and exit_code too
	cmd         *model.Command
	shell, dir  string
	base        string
	env         model.Env
	pass        []string
	stdin       *model.Stdin
	inputs      map[string]any // INPUT_*, only for the program of a task
	outputs     map[string]*model.Output
	retry       *model.Retry
	failedWhen  any
	changedWhen any
	stop        *model.Stop
	unit        *instance  // for the task of a unit
	call        bool       // a call of command, whose outputs are its streams
	plugin      *pluginRun // for the programs of a plugin task
}

// program runs the program of a task or a step as often as retry allows;
// see try.
func (e *engine) program(ctx context.Context, p progSpec, s *expr.Scope) *taskResult {
	return e.try(ctx, p, s, func(s *expr.Scope) (*taskResult, map[string]any, error) {
		c, err := e.programCall(p, s)
		if err != nil {
			return nil, nil, err
		}
		if c.Command, err = command(p.cmd, s); err != nil {
			return nil, nil, fmt.Errorf("run: %w", err)
		}
		return e.execProgram(ctx, p, c)
	})
}

// try runs attempt as often as retry allows, and judges each with
// failed_when and changed_when, which read the outputs it judges by. Each
// attempt evaluates its own expressions, and after the first with kv()
// read again, so one after a failure reads what changed since.
func (e *engine) try(ctx context.Context, p progSpec, s *expr.Scope, attempt func(*expr.Scope) (*taskResult, map[string]any, error)) *taskResult {
	fail := func(err error) *taskResult {
		e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: err.Error()})
		return failed(err)
	}
	n, delay, backoff := 1, time.Duration(0), 1.0
	if r := p.retry; r != nil {
		n = max(1, r.Attempts)
		if r.Delay != "" {
			var err error
			if delay, err = time.ParseDuration(r.Delay); err != nil {
				return fail(fmt.Errorf("retry.delay: %w", err))
			}
		}
		if r.Backoff > 0 {
			backoff = r.Backoff
		}
	}
	for i := 1; ; i++ {
		if i > 1 {
			s = e.newKV().scope(s)
		}
		r, judged, err := attempt(s)
		if err != nil {
			return fail(err)
		}
		js := s.With("outputs", judged)
		r = judge(r, p.failedWhen, p.changedWhen, js)
		done := r.outcome == success
		if p.retry != nil && p.retry.Until != nil {
			ok, err := expr.EvalCondition(p.retry.Until, js)
			if err != nil {
				return fail(fmt.Errorf("retry.until: %w", err))
			}
			done = ok
		}
		if done || i >= n || ctx.Err() != nil {
			if !done && r.outcome == success {
				r.outcome = failure
			}
			return r
		}
		e.sayTask(p.label, p.step, "attempt %d of %d did not succeed; again in %s", i, n, delay)
		select {
		case <-ctx.Done():
			return r
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * backoff)
	}
}

// programCall evaluates what p runs with: its directory, environment,
// stdin, and the variables pass lets through; the caller sets the command.
func (e *engine) programCall(p progSpec, s *expr.Scope) (*plugin.Call, error) {
	dir, err := expr.EvalText(p.dir, s)
	if err != nil {
		return nil, fmt.Errorf("dir: %w", err)
	}
	env := map[string]model.EnvValue{}
	for k, v := range p.env {
		text, err := expr.EvalText(v.Text, s)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		env[k] = model.EnvValue{Text: text, File: v.File}
	}
	var stdin []byte
	if p.stdin != nil {
		v, err := expr.EvalValue(p.stdin.Value, s)
		if err == nil {
			stdin, err = plugin.Encode(v, p.stdin.Format)
		}
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
	}
	stop, err := plugin.StopOf(p.stop)
	if err != nil {
		return nil, err
	}
	var pass []string
	for _, kv := range e.opts.Environ {
		k, _, _ := strings.Cut(kv, "=")
		for _, pat := range p.pass {
			if ok, _ := path.Match(pat, k); ok && !model.ReservedEnv(k) && !slices.Contains(pass, k) {
				pass = append(pass, k)
			}
		}
	}
	var unit *plugin.Unit
	if p.unit != nil {
		unit = &plugin.Unit{Address: p.unit.address}
		if p.unit.record != nil {
			unit.StateKey = p.unit.record.StateKey
		}
	}
	c := &plugin.Call{Shell: p.shell, Base: p.base, Dir: dir, Env: env, Pass: pass, Stdin: stdin, Stop: stop,
		Task: p.task, Reason: "requested", Unit: unit, Inputs: p.inputs, Outputs: p.outputs}
	if p.plugin != nil {
		c.PluginDir = p.plugin.dir
	}
	return c, nil
}

// execProgram runs c once: its streams go to the log, and the outputs it
// is judged by add stdout, stderr, and exit_code to the declared ones,
// which are also the outputs of a step and of a call of command.
func (e *engine) execProgram(ctx context.Context, p progSpec, c *plugin.Call) (*taskResult, map[string]any, error) {
	addr := ""
	if c.Unit != nil {
		addr = c.Unit.Address
	}
	out := &lineWriter{e: e, addr: addr, task: p.label, step: p.step, stream: "stdout"}
	errw := &lineWriter{e: e, addr: addr, task: p.label, step: p.step, stream: "stderr"}
	c.Stdout, c.Stderr = out, errw
	res, err := e.run.Exec(ctx, c)
	out.flush()
	errw.flush()
	if err != nil {
		return nil, nil, err
	}
	for _, w := range res.Warnings {
		e.emit(Event{Type: "diagnostic", Level: "warn", Address: addr, Task: p.label, Step: p.step, Message: w})
	}
	judged := maps.Clone(res.Outputs)
	judged["stdout"], judged["stderr"], judged["exit_code"] = res.Stdout, res.Stderr, int64(res.ExitCode)
	r := &taskResult{outcome: success, changed: res.Changed, outputs: res.Outputs}
	if p.step != "" || p.call {
		r.outputs = judged
	}
	if err := outcomeErr(res); err != nil {
		r.outcome = failure
		if p.failedWhen == nil {
			e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: err.Error()})
		}
	}
	return r, judged, nil
}
