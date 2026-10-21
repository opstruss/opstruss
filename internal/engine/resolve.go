package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// resolved is a run unit with every expression evaluated.
type resolved struct {
	cmd      model.Command
	shell    string
	dir      string // working directory relative to the unit's file, as written
	cwd      string
	env      map[string]model.EnvValue
	pass     map[string]string // the operator's variables that pass lets through
	stdin    []byte
	stdinVal any
	revision string
	down     *model.Command
	requires []string
	timeout  time.Duration
	stop     plugin.Stop
	waitFor  string
	plugin   *pluginUnit // for a unit that uses a plugin
	task     *taskUnit   // for a unit that uses a task
}

// resolve evaluates the expressions of inst. In a preview, a unit that
// reads outputs this run changes first is not evaluated: waitFor names the
// upstream unit, and the unit is planned at its turn.
func (e *engine) resolve(inst *instance, preview bool) (*resolved, error) {
	u := inst.def
	r := &resolved{shell: u.Shell, timeout: DefaultTimeout}
	if preview {
		for _, v := range []any{argvOf(u.Run), argvOf(u.Down), envOf(u.Env), u.Dir, stdinOf(u.Stdin), u.Revision, anyMap(u.With)} {
			if up := e.changing(v, inst.frame); up != "" {
				r.waitFor = up
				return r, nil
			}
		}
		for f := inst.frame; f.node != nil; f = f.parent {
			if up := e.changing(envOf(f.node.Unit.Env), f.parent); up != "" {
				r.waitFor = up
				return r, nil
			}
		}
	}
	s := e.scope(inst)
	var err error
	if r.cmd, err = command(u.Run, s); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	if u.Down != nil {
		cmd, err := command(u.Down, s)
		if err != nil {
			return nil, fmt.Errorf("down: %w", err)
		}
		r.down = &cmd
	}
	if r.dir, err = expr.EvalText(u.Dir, s); err != nil {
		return nil, fmt.Errorf("dir: %w", err)
	}
	r.cwd = filepath.Join(inst.dir, r.dir)
	if filepath.IsAbs(r.dir) {
		r.cwd = r.dir
	}
	r.env = map[string]model.EnvValue{}
	if err := e.frameEnv(inst.frame, r.env); err != nil {
		return nil, err
	}
	for k, v := range u.Env {
		text, err := expr.EvalText(v.Text, s)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", k, err)
		}
		r.env[k] = model.EnvValue{Text: text, File: v.File}
	}
	r.pass = map[string]string{}
	for _, kv := range e.opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		for _, p := range u.Pass {
			if ok, _ := path.Match(p, k); ok && !model.ReservedEnv(k) {
				r.pass[k] = v
			}
		}
	}
	if u.Stdin != nil {
		if r.stdinVal, err = expr.EvalValue(u.Stdin.Value, s); err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		if r.stdin, err = plugin.Encode(r.stdinVal, u.Stdin.Format); err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
	}
	if r.revision, err = expr.EvalText(u.Revision, s); err != nil {
		return nil, fmt.Errorf("revision: %w", err)
	}
	if r.requires, err = requires(u.Requires, s); err != nil {
		return nil, fmt.Errorf("requires: %w", err)
	}
	if u.Timeout != "" {
		if r.timeout, err = time.ParseDuration(u.Timeout); err != nil {
			return nil, fmt.Errorf("timeout: %w", err)
		}
	}
	if r.stop, err = plugin.StopOf(u.Stop); err != nil {
		return nil, err
	}
	switch t := u.Target; {
	case t == nil:
	case t.Kind == model.TargetPlugin:
		if err := e.resolvePlugin(inst, r, s); err != nil {
			return nil, err
		}
	case t.Kind != model.TargetComponent:
		if err := e.resolveTask(inst, r, s); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func command(c *model.Command, s *expr.Scope) (model.Command, error) {
	if c == nil {
		return model.Command{}, nil
	}
	if c.Script != "" {
		// validate leaves only $${{ in a script, which stands for ${{.
		text, err := expr.EvalText(c.Script, s)
		return model.Command{Script: text}, err
	}
	var argv []string
	for i, a := range c.Argv {
		text, err := expr.EvalText(a, s)
		if err != nil {
			return model.Command{}, fmt.Errorf("item %d: %w", i, err)
		}
		argv = append(argv, text)
	}
	return model.Command{Argv: argv}, nil
}

func requires(r *model.Requires, s *expr.Scope) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	if r.Expr != "" {
		v, err := expr.EvalString(r.Expr, s)
		if err != nil {
			return nil, err
		}
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("gives %s, not a list of programs", model.TypeName(v))
		}
		var out []string
		for _, it := range list {
			name, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("lists %s, not a program name", model.TypeName(it))
			}
			out = append(out, name)
		}
		return out, nil
	}
	var out []string
	for _, it := range r.List {
		name, err := expr.EvalText(it, s)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

func argvOf(c *model.Command) any {
	if c == nil {
		return nil
	}
	if c.Script != "" {
		return c.Script
	}
	list := make([]any, len(c.Argv))
	for i, a := range c.Argv {
		list[i] = a
	}
	return list
}

func envOf(env model.Env) any {
	m := map[string]any{}
	for k, v := range env {
		m[k] = v.Text
	}
	return m
}

func stdinOf(s *model.Stdin) any {
	if s == nil {
		return nil
	}
	return s.Value
}

func cmdValue(c model.Command) any {
	if c.Script != "" {
		return c.Script
	}
	list := make([]any, len(c.Argv))
	for i, a := range c.Argv {
		list[i] = a
	}
	return list
}

// kind is what the record says the unit is.
func (r *resolved) kind() string {
	switch {
	case r.plugin != nil:
		return "plugin"
	case r.task != nil:
		return "task"
	}
	return "run"
}

// inputs are the plugin's inputs, which the record keeps for down and
// $OPSTRUSS_STATE.
func (r *resolved) inputs() map[string]any {
	switch {
	case r.plugin != nil:
		return r.plugin.inputs
	case r.task != nil:
		return r.task.inputs
	}
	return nil
}

// source is where the programs of a unit that uses a plugin come from.
func (r *resolved) source(inst *instance) *state.Source {
	switch {
	case r.plugin != nil:
		return &state.Source{Uses: inst.def.Uses, Hash: r.plugin.hash}
	case r.task != nil:
		return &state.Source{Uses: inst.def.Uses, Hash: r.task.hash}
	}
	return nil
}

// fingerprint is the sha256 of a pass value, which down compares with the
// value it then finds.
func fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *resolved) fingerprints() map[string]string {
	var out map[string]string
	for k, v := range r.pass {
		if out == nil {
			out = map[string]string{}
		}
		out[k] = fingerprint(v)
	}
	return out
}

// downContract is what down runs for the unit, with the context it needs.
func (e *engine) downContract(inst *instance, r *resolved) *state.Down {
	if r.down == nil {
		return nil
	}
	rel, err := filepath.Rel(e.p.Dir, r.cwd)
	if err != nil {
		rel = r.cwd
	}
	d := &state.Down{Run: cmdValue(*r.down), Shell: r.shell, Dir: filepath.ToSlash(rel), Plugin: e.pluginOf(r),
		Tool: r.plugin != nil && r.plugin.tool != "", Pass: r.fingerprints(), Timeout: inst.def.Timeout}
	for k, v := range r.env {
		if v.File {
			if d.EnvFiles == nil {
				d.EnvFiles = map[string]string{}
			}
			d.EnvFiles[k] = v.Text
		} else {
			if d.Env == nil {
				d.Env = map[string]string{}
			}
			d.Env[k] = v.Text
		}
	}
	if s := inst.def.Stop; s != nil {
		d.Stop = &state.DownStop{Signal: s.Signal, Grace: s.Grace}
	}
	return d
}

// digest is the digest trigger of a run unit: sha256 over the canonical
// JSON of what decides its result, the files of sources outside generates
// and a relative program included. The base environment, timeout, retry,
// approval, and down are left out, since they do not change the result.
func (e *engine) digest(inst *instance, r *resolved) (string, []string, error) {
	sources, missing, err := files(inst.dir, inst.def.Sources, inst.def.Generates)
	if err != nil {
		return "", nil, err
	}
	def := map[string]any{"kind": "run", "run": cmdValue(r.cmd)}
	if r.plugin != nil {
		def["kind"], def["uses"], def["source"], def["with"] = "plugin", inst.def.Uses, r.plugin.hash, r.plugin.inputs
	}
	if r.task != nil {
		t := r.task.target
		def["kind"], def["uses"], def["with"], def["task"] = "task", inst.def.Uses, r.task.inputs, t.Task
		switch t.Kind {
		case model.TargetProject:
			src := map[string]any{}
			if err := e.taskSource(t.Task, src); err != nil {
				return "", nil, err
			}
			def["source"] = src
		case model.TargetTask:
			def["source"] = r.task.hash
		}
	}
	add := func(k string, v any, empty bool) {
		if !empty {
			def[k] = v
		}
	}
	shell := r.shell
	if r.cmd.Script != "" && shell == "" {
		shell = "sh"
	}
	rel, _ := filepath.Rel(e.p.Dir, r.cwd)
	add("shell", shell, r.cmd.Script == "")
	add("dir", filepath.ToSlash(rel), rel == ".")
	env := map[string]any{}
	for k, v := range r.env {
		if v.File {
			env[k] = map[string]any{"file": v.Text}
		} else {
			env[k] = v.Text
		}
	}
	add("env", env, len(env) == 0)
	prints := map[string]any{}
	for k, v := range r.fingerprints() {
		prints[k] = v
	}
	add("pass", prints, len(prints) == 0)
	if inst.def.Stdin != nil {
		def["stdin"] = map[string]any{"value": r.stdinVal, "format": inst.def.Stdin.Format}
	}
	outputs := map[string]any{}
	for k, o := range inst.def.Outputs {
		outputs[k] = o
	}
	add("outputs", outputs, len(outputs) == 0)
	if len(r.cmd.Argv) > 0 && strings.Contains(r.cmd.Argv[0], "/") && r.plugin == nil {
		prog := r.cmd.Argv[0]
		if !filepath.IsAbs(prog) {
			prog = filepath.Join(r.cwd, prog)
		}
		sum, err := hashFile(prog)
		if err != nil {
			return "", nil, fmt.Errorf("program: %w", err)
		}
		def["program"] = sum
	}
	add("sources", sources, len(sources) == 0)
	add("revision", r.revision, r.revision == "")
	data, err := canonical(def)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return "sha256:v1:" + hex.EncodeToString(sum[:]), missing, nil
}

// files hashes the files of sources, relative to dir, without those that
// match generates, and lists the generates that are missing: a path that
// is not there, or a pattern that matches no file.
func files(dir string, sources []model.PathItem, generates []string) (map[string]any, []string, error) {
	made := map[string]bool{}
	var missing []string
	for _, g := range generates {
		matches, err := glob(dir, g)
		if err != nil {
			return nil, nil, fmt.Errorf("generates: %w", err)
		}
		if len(matches) == 0 {
			missing = append(missing, g)
		}
		for _, m := range matches {
			made[m] = true
		}
	}
	set := map[string]bool{}
	for _, item := range sources {
		if item.Exclude {
			for p := range set {
				if ok, _ := doublestar.PathMatch(filepath.Join(dir, item.Pattern), p); ok {
					delete(set, p)
				}
			}
			continue
		}
		matches, err := glob(dir, item.Pattern)
		if err != nil {
			return nil, nil, fmt.Errorf("sources: %w", err)
		}
		for _, m := range matches {
			set[m] = true
		}
	}
	out := map[string]any{}
	for _, p := range slices.Sorted(maps.Keys(set)) {
		if made[p] {
			continue
		}
		sum, err := hashFile(p)
		if err != nil {
			return nil, nil, err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = sum
	}
	return out, missing, nil
}

// glob returns the files that pattern, relative to dir, matches.
func glob(dir, pattern string) ([]string, error) {
	return doublestar.FilepathGlob(filepath.Join(dir, pattern), doublestar.WithFilesOnly())
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
