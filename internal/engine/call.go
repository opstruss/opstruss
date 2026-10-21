package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/opstruss/opstruss/internal/builtin"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// callByName runs a call of a task by name, in a task or a step, as often
// as retry allows, with its inputs evaluated at each attempt.
func (e *engine) callByName(ctx context.Context, p progSpec, name string, args map[string]any, s *expr.Scope) *taskResult {
	t, err := e.p.Resolve(name)
	if err != nil {
		e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: err.Error()})
		return failed(err)
	}
	if t.Package != nil && t.Package.Decl != nil {
		// The env and pass of a declaration go to what it declares, under
		// those of the call.
		env := maps.Clone(t.Package.Decl.Env)
		if env == nil {
			env = model.Env{}
		}
		maps.Copy(env, p.env)
		p.env, p.pass = env, append(append([]string{}, t.Package.Decl.Pass...), p.pass...)
	}
	return e.try(ctx, p, s, func() (*taskResult, map[string]any, error) {
		in, err := callInputs(t, args, s)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if t.Task == "command" {
			return e.commandCall(ctx, p, in, s)
		}
		dir, err := expr.EvalText(p.dir, s)
		if err != nil {
			return nil, nil, fmt.Errorf("dir: %w", err)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(p.base, dir)
		}
		res := builtin.Run(ctx, &builtin.Call{Task: t.Task, Inputs: in, Dir: dir, Log: func(level, msg string) {
			e.emit(Event{Type: "log", Level: level, Task: p.label, Step: p.step, Message: msg})
		}})
		r := &taskResult{outcome: success, changed: res.Changed, outputs: res.Outputs}
		if res.Err != nil {
			r.outcome = failure
			if p.failedWhen == nil {
				e.emit(Event{Type: "step", Level: "error", Task: p.label, Step: p.step, Message: fmt.Sprintf("%s: %v", name, res.Err)})
			}
		}
		return r, res.Outputs, nil
	})
}

// callInputs gives a call its inputs: those of the declaration's with:
// that the task takes, then the call's own, evaluated and checked against
// the task's schema.
func callInputs(t *model.Target, args map[string]any, s *expr.Scope) (map[string]any, error) {
	decl, typed := t.Inputs()
	given := map[string]any{}
	if t.Package != nil && t.Package.Decl != nil {
		for k, v := range t.Package.Decl.With {
			if !typed || decl[k] != nil {
				given[k] = v
			}
		}
	}
	maps.Copy(given, args)
	out := map[string]any{}
	for k, v := range given {
		val, err := expr.EvalValue(v, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = val
	}
	if !typed {
		return out, nil
	}
	return conform(decl, out)
}

// commandCall runs a call of command through the program contract, as run
// does, with argv or script, shell, and stdin from its inputs.
func (e *engine) commandCall(ctx context.Context, p progSpec, in map[string]any, s *expr.Scope) (*taskResult, map[string]any, error) {
	if (in["argv"] == nil) == (in["script"] == nil) {
		return nil, nil, errors.New("command takes exactly one of argv or script")
	}
	if sh, ok := in["shell"].(string); ok {
		p.shell = sh
	}
	c, err := e.programCall(p, s)
	if err != nil {
		return nil, nil, err
	}
	if script, ok := in["script"].(string); ok {
		c.Command = model.Command{Script: script}
	} else {
		for _, a := range in["argv"].([]any) {
			c.Command.Argv = append(c.Command.Argv, a.(string))
		}
	}
	if st, ok := in["stdin"].(map[string]any); ok {
		format, _ := st["format"].(string)
		if c.Stdin, err = plugin.Encode(st["value"], format); err != nil {
			return nil, nil, fmt.Errorf("stdin: %w", err)
		}
	}
	p.call = true
	return e.execProgram(ctx, p, c)
}
