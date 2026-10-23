package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// Stdio is the terminal that opstruss exec attaches a program to.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// Exec runs argv, a program of the operator, with what the programs of the
// unit named in opts.Units get, under the unit's lock and attached to the
// terminal: through the exec hook of the unit's plugin when it has one,
// which gets argv after its own arguments, and as it is otherwise. It is
// no task, so no plan, approval, or protect applies, and its output stays
// out of the events. The record of the run names it <unit>/exec, with argv
// and the program's exit code, which is the run's.
func Exec(ctx context.Context, opts Options, argv []string, tty Stdio) (*Result, error) {
	if len(opts.Units) != 1 || strings.Contains(opts.Units[0], "*") {
		return nil, errors.New("exec runs for one unit, named by its address without the stack, as opstruss exec dns/zone -- <program>")
	}
	if len(argv) == 0 {
		return nil, errors.New("name the program after --, as opstruss exec <unit> -- <program>")
	}
	unit := opts.Units[0]
	opts.Units = append([]string{unit, "--"}, argv...)
	e, err := open(ctx, opts, "exec", unit+"/exec")
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	ctx = e.ctx
	code, err := e.execUnit(ctx, unit, argv, tty)
	if err != nil {
		e.fail(e.p.StackName+"/"+unit, err)
	} else {
		e.exit = code
	}
	outcome := success
	if e.exit != 0 {
		outcome = failure
	}
	e.close(outcome, nil, nil)
	return e.result(), nil
}

// execUnit finds the instance at unit, takes its lock, and runs argv in
// its place.
func (e *engine) execUnit(ctx context.Context, unit string, argv []string, tty Stdio) (int, error) {
	e.passMu.Lock()
	e.pass = pass{units: []string{unit}, noDeps: true}
	e.resetUnits()
	err := e.selectUnits(false)
	var inst *instance
	if err == nil {
		e.previewQuiet()
		for _, i := range e.insts {
			if e.short(i.address) == unit {
				inst = i
			}
		}
	}
	e.passMu.Unlock()
	switch n := e.p.NodeAt(unit); {
	case err != nil:
		return 0, err
	case inst == nil && n != nil && n.Component() != nil:
		return 0, fmt.Errorf("%s uses a component; exec runs for one unit inside it, as %s/<unit>", unit, unit)
	case inst == nil:
		return 0, fmt.Errorf("there is no instance at %s; name one, as <unit>[<key>]", unit)
	}
	token, err := e.lock(ctx, inst.address)
	if err != nil {
		return 0, err
	}
	defer func() { _ = e.store.Unlock(inst.address, token) }()
	rec, _, err := e.store.Get(inst.address)
	if err == nil {
		err = tombstoned(rec)
	}
	if err != nil {
		return 0, err
	}
	e.setRecord(inst, rec)
	r, err := e.resolve(inst, false)
	if err != nil {
		return 0, err
	}
	if err := e.missing(inst, r); err != nil {
		return 0, err
	}
	c := &plugin.Call{Command: model.Command{Argv: argv}, Base: inst.dir, Dir: r.dir, Env: r.env,
		Pass: slices.Sorted(maps.Keys(r.pass)), Task: "exec", Reason: "requested", Unit: e.unitOf(inst, r, rec)}
	switch {
	case r.plugin != nil:
		c.Inputs, c.Env = r.plugin.inputs, r.plugin.env
		if hook := r.plugin.target.Package.Plugin.Tasks["exec"]; hook != nil {
			if err := e.execHook(c, inst, r, hook, argv); err != nil {
				return 0, err
			}
		}
	case r.task != nil:
		c.Inputs = r.task.inputs
	}
	return e.run.Attach(c, tty.In, tty.Out, tty.Err)
}

// execHook makes c run the exec hook of the unit's plugin with argv after
// its own arguments: from the plugin's directory, in the unit's working
// directory and its own dir:, with its env under the unit's.
func (e *engine) execHook(c *plugin.Call, inst *instance, r *resolved, hook *model.Task, argv []string) error {
	ps := e.pluginScope(r.plugin.dir, r.plugin.inputs, nil, inst)
	cmd, err := e.command(hook.Run, ps)
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	dir := r.plugin.base
	if hook.Dir != "" {
		d, err := expr.EvalText(hook.Dir, ps)
		if err != nil {
			return fmt.Errorf("exec: dir: %w", err)
		}
		dir = filepath.Join(dir, d)
	}
	env := map[string]model.EnvValue{}
	for k, v := range hook.Env {
		text, err := expr.EvalText(v.Text, ps)
		if err != nil {
			return fmt.Errorf("exec: env.%s: %w", k, err)
		}
		env[k] = model.EnvValue{Text: text, File: v.File}
	}
	maps.Copy(env, r.plugin.env)
	c.Command, c.Shell, c.Args, c.Dir, c.Env, c.PluginDir = cmd, hook.Shell, argv, dir, env, r.plugin.dir
	return nil
}
