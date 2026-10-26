package engine

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"github.com/opstruss/opstruss/internal/sources"
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
	destroy  *model.Command
	requires []string
	timeout  time.Duration
	stop     plugin.Stop
	waitFor  string
	reads    []string    // the parts that read outputs this run changes first, such as env.URL
	plugin   *pluginUnit // for a unit that uses a plugin
	task     *taskUnit   // for a unit that uses a task
	service  *serviceDef // for a service unit

	// What is sensitive: inputs of the plugin or task, variables of env,
	// and stdin. The record keeps them encrypted and the digest hashes them
	// with a key of its own.
	inputHidden    map[string]bool
	envHidden      map[string]bool
	stdinSensitive bool
}

// resolve evaluates the expressions of inst. In a preview, a unit that
// reads outputs this run changes first is not evaluated: waitFor names the
// first upstream unit, reads the parts that read such outputs, and the
// unit is planned at its turn.
func (e *engine) resolve(inst *instance, preview bool) (*resolved, error) {
	u := inst.def
	r := &resolved{shell: u.Shell, timeout: DefaultTimeout}
	if preview {
		reads := func(name string, v any, f *frame) {
			if up := e.changing(v, f); up != "" {
				r.waitFor = cmp.Or(r.waitFor, up)
				r.reads = append(r.reads, name)
			}
		}
		reads("run", argvOf(u.Run), inst.frame)
		reads("service", argvOf(u.Service), inst.frame)
		reads("destroy", argvOf(u.Destroy), inst.frame)
		for _, k := range slices.Sorted(maps.Keys(u.Env)) {
			reads("env."+k, u.Env[k].Text, inst.frame)
		}
		reads("dir", u.Dir, inst.frame)
		reads("stdin", stdinOf(u.Stdin), inst.frame)
		reads("revision", u.Revision, inst.frame)
		for _, k := range slices.Sorted(maps.Keys(u.With)) {
			reads("with."+k, u.With[k], inst.frame)
		}
		reads("ready", readyOf(u.Ready), inst.frame)
		reads("stop.run", stopRunOf(u.Stop), inst.frame)
		for f := inst.frame; f.node != nil; f = f.parent {
			for _, k := range slices.Sorted(maps.Keys(f.node.Unit.Env)) {
				if !slices.Contains(r.reads, "env."+k) {
					reads("env."+k, f.node.Unit.Env[k].Text, f.parent)
				}
			}
		}
		if r.waitFor != "" {
			return r, nil
		}
	}
	s := e.scope(inst)
	var err error
	if r.cmd, err = e.command(u.Run, s); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	if u.Destroy != nil {
		cmd, err := e.command(u.Destroy, s)
		if err != nil {
			return nil, fmt.Errorf("destroy: %w", err)
		}
		r.destroy = &cmd
	}
	if r.dir, err = expr.EvalText(u.Dir, s); err != nil {
		return nil, fmt.Errorf("dir: %w", err)
	}
	r.cwd = filepath.Join(inst.dir, r.dir)
	if filepath.IsAbs(r.dir) {
		r.cwd = r.dir
	}
	r.env, r.envHidden, r.inputHidden = map[string]model.EnvValue{}, map[string]bool{}, map[string]bool{}
	if err := e.frameEnv(inst.frame, r.env, r.envHidden); err != nil {
		return nil, err
	}
	for k, v := range u.Env {
		if err := r.evalEnv(e, k, v, s); err != nil {
			return nil, err
		}
	}
	r.pass = map[string]string{}
	for _, kv := range e.opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		for _, p := range u.Pass {
			if ok, _ := path.Match(p, k); ok && !model.ReservedEnv(k) && inst.node.Grants(k) {
				r.pass[k] = v
			}
		}
	}
	if u.Stdin != nil {
		var marks any
		if r.stdinVal, marks, err = expr.EvalMarked(u.Stdin.Value, s); err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		if marks != nil || e.redact.Contains(r.stdinVal) {
			r.stdinSensitive = true
			e.hide(r.stdinVal, true)
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
	if u.Service != nil {
		if err := e.resolveService(u, r, s); err != nil {
			return nil, err
		}
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

// errArgv refuses a sensitive value in argv, which validate does too for
// what it knows from the files.
var errArgv = errors.New("a sensitive value cannot be an item of argv, which other users can read in the list of processes; pass it through env or stdin")

// command evaluates a program: a script as it is, or each item of argv,
// none of which may hold a sensitive value.
func (e *engine) command(c *model.Command, s *expr.Scope) (model.Command, error) {
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
		text, sensitive, err := expr.EvalTextMarked(a, s)
		if err != nil {
			return model.Command{}, fmt.Errorf("item %d: %w", i, err)
		}
		// A value that the run knows as sensitive and that an expression put
		// in the item would show in the list of processes. Text that the
		// file itself writes, such as "token" in a script while a secret's
		// value is token, is not such a value.
		if sensitive || e.redact.Contains(text) && !e.redact.Contains(a) {
			return model.Command{}, fmt.Errorf("item %d: %w", i, errArgv)
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

func stdinOf(s *model.Stdin) any {
	if s == nil {
		return nil
	}
	return s.Value
}

func readyOf(r *model.Ready) any {
	if r == nil {
		return nil
	}
	return []any{r.HTTP, r.TCP, argvOf(r.Run)}
}

func stopRunOf(s *model.Stop) any {
	if s == nil {
		return nil
	}
	return argvOf(s.Run)
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
	case r.plugin != nil, r.task != nil:
		return "uses"
	case r.service != nil:
		return "service"
	}
	return "run"
}

// inputs are the plugin's inputs, which the record keeps for destroy and
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

// evalEnv evaluates the variable k of env in s, noting whether it is
// sensitive.
func (r *resolved) evalEnv(e *engine, k string, v model.EnvValue, s *expr.Scope) error {
	text, sensitive, err := expr.EvalTextMarked(v.Text, s)
	if err != nil {
		return fmt.Errorf("env.%s: %w", k, err)
	}
	r.env[k] = model.EnvValue{Text: text, File: v.File}
	r.envHidden[k] = sensitive
	if sensitive {
		e.hide(text, true)
	}
	return nil
}

// hidden names what the record of the unit keeps encrypted: its sensitive
// inputs, and the sensitive variables of the env of destroy d.
func (r *resolved) hidden(d *state.Destroy) map[string][]string {
	out := map[string][]string{}
	for _, k := range slices.Sorted(maps.Keys(r.inputs())) {
		if r.inputHidden[k] {
			out[state.PartInputs] = append(out[state.PartInputs], k)
		}
	}
	if d != nil {
		for _, k := range slices.Sorted(maps.Keys(r.envHidden)) {
			switch _, file := d.EnvFiles[k]; {
			case !r.envHidden[k]:
			case file:
				out[state.PartEnvFiles] = append(out[state.PartEnvFiles], k)
			default:
				if _, ok := d.Env[k]; ok {
					out[state.PartEnv] = append(out[state.PartEnv], k)
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sensitive reports whether the unit has a sensitive value of its own,
// which makes its record keep raw_output encrypted too.
func (r *resolved) sensitive() bool {
	for _, h := range []map[string]bool{r.inputHidden, r.envHidden} {
		for _, v := range h {
			if v {
				return true
			}
		}
	}
	return r.stdinSensitive
}

// source is where the programs of a unit that uses a plugin come from.
func (r *resolved) source(inst *instance) *state.Source {
	if r.plugin != nil && r.plugin.target.Package.Source != nil {
		return sourceOf(r.plugin.target.Package.Source)
	}
	if r.task != nil && r.task.target.Package != nil && r.task.target.Package.Source != nil {
		return sourceOf(r.task.target.Package.Source)
	}
	if inst.node.In.Source != nil {
		return sourceOf(inst.node.In.Source)
	}
	switch {
	case r.plugin != nil:
		return &state.Source{Uses: inst.def.Uses, Hash: r.plugin.hash}
	case r.task != nil:
		return &state.Source{Uses: inst.def.Uses, Hash: r.task.hash}
	}
	return nil
}

// fingerprint is the sha256 of a pass value, which destroy compares with the
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

// destroyContract is what destroy runs for the unit, with the context it needs.
func (e *engine) destroyContract(inst *instance, r *resolved) *state.Destroy {
	if r.service != nil {
		d := r.service.destroy(r)
		e.sourceContract(inst, r, d)
		return d
	}
	if r.destroy == nil {
		return nil
	}
	rel, err := filepath.Rel(e.p.Dir, r.cwd)
	if err != nil {
		rel = r.cwd
	}
	d := &state.Destroy{Run: cmdValue(*r.destroy), Shell: r.shell, Dir: filepath.ToSlash(rel), Plugin: e.pluginOf(r),
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
		d.Stop = &state.DestroyStop{Signal: s.Signal, Grace: s.Grace}
	}
	e.sourceContract(inst, r, d)
	return d
}

// digested is what digest finds of a unit.
type digested struct {
	digest  string
	parts   map[string]string // the digest of each part, by its name
	missing []string          // the generates that are missing
	empty   []string          // the patterns of sources that match no file
}

// digest is the digest trigger of a run unit: sha256 over the canonical
// JSON of what decides its result, the files of sources outside generates
// and a relative program included. The base environment, timeout, retry,
// approval, and destroy are left out, since they do not change the result.
func (e *engine) digest(inst *instance, r *resolved) (digested, error) {
	sources, missing, empty, err := files(inst.dir, inst.def.Sources, inst.def.Generates)
	if err != nil {
		return digested{}, err
	}
	with := maps.Clone(r.inputs())
	for k := range with {
		if r.inputHidden[k] {
			if with[k], err = e.hashed(inst, with[k]); err != nil {
				return digested{}, err
			}
		}
	}
	def := map[string]any{"kind": "run", "run": cmdValue(r.cmd)}
	if r.plugin != nil {
		def["kind"], def["uses"], def["source"], def["with"] = "plugin", inst.def.Uses, r.plugin.hash, with
	}
	if r.task != nil {
		t := r.task.target
		def["kind"], def["uses"], def["with"], def["task"] = "task", inst.def.Uses, with, t.Task
		switch t.Kind {
		case model.TargetProject:
			src := map[string]any{}
			if err := e.taskSource(inst.node.In, t.Task, src); err != nil {
				return digested{}, err
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
	if id, _, err := e.locationOwner(inst, r, r.cwd); err != nil {
		return digested{}, err
	} else if id != nil {
		add("dir", digestSource(id, ""), false)
	} else {
		add("dir", filepath.ToSlash(rel), rel == ".")
	}
	if in := inst.node.In; in.Source != nil {
		h, err := e.packageHash(in.Source, in.Catalog, in.Dir)
		if err != nil {
			return digested{}, err
		}
		def["owner"] = digestSource(in.Source, h)
	}
	if r.plugin != nil && r.plugin.target.Package.Source != nil {
		id := r.plugin.target.Package.Source
		def["uses"], def["source"] = id.Address(), digestSource(id, r.plugin.hash)
	}
	if r.task != nil && r.task.target.Package != nil && r.task.target.Package.Source != nil {
		id := r.task.target.Package.Source
		def["uses"], def["source"] = id.Address(), digestSource(id, r.task.hash)
	}
	env := map[string]any{}
	for k, v := range r.env {
		var text any = v.Text
		if r.envHidden[k] {
			if text, err = e.hashed(inst, v.Text); err != nil {
				return digested{}, err
			}
		}
		if v.File {
			env[k] = map[string]any{"file": text}
		} else {
			env[k] = text
		}
	}
	add("env", env, len(env) == 0)
	prints := map[string]any{}
	for k, v := range r.fingerprints() {
		prints[k] = v
	}
	add("pass", prints, len(prints) == 0)
	if inst.def.Stdin != nil {
		v := r.stdinVal
		if r.stdinSensitive {
			if v, err = e.hashed(inst, v); err != nil {
				return digested{}, err
			}
		}
		def["stdin"] = map[string]any{"value": v, "format": inst.def.Stdin.Format}
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
			return digested{}, fmt.Errorf("program: %w", err)
		}
		def["program"] = sum
	}
	add("sources", sources, len(sources) == 0)
	add("revision", r.revision, r.revision == "")
	if r.service != nil {
		r.service.digest(r, def)
	}
	data, err := canonical(def)
	if err != nil {
		return digested{}, err
	}
	parts, err := partsOf(def)
	if err != nil {
		return digested{}, err
	}
	sum := sha256.Sum256(data)
	return digested{digest: "sha256:v1:" + hex.EncodeToString(sum[:]), parts: parts, missing: missing, empty: empty}, nil
}

// partsOf gives each part of def a digest of its own, which a record keeps
// so a plan names what changed, as bazel --explain does: an input of
// with, a variable of env or pass, and each other key, such as run or
// sources, with plugin for the files of a plugin and component for those
// of the package a unit is in. Sensitive values are hashed already in def.
func partsOf(def map[string]any) (map[string]string, error) {
	out := map[string]string{}
	add := func(name string, v any) error {
		data, err := canonical(v)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[name] = hex.EncodeToString(sum[:8])
		return nil
	}
	for k, v := range def {
		m, ok := v.(map[string]any)
		if !ok || k != "with" && k != "env" && k != "pass" {
			name := k
			switch {
			case k == "owner":
				name = "component"
			case k == "source" && def["kind"] == "plugin":
				name = "plugin"
			}
			if err := add(name, v); err != nil {
				return nil, err
			}
			continue
		}
		for name, x := range m {
			if err := add(k+"."+name, x); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// changedParts names the parts whose digests differ between was and now,
// or that only one has, sorted; nil when was is nil, as in a record that
// keeps no parts.
func changedParts(was, now map[string]string) []string {
	if was == nil {
		return nil
	}
	var out []string
	for k, v := range now {
		if was[k] != v {
			out = append(out, k)
		}
	}
	for k := range was {
		if _, ok := now[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// digestSource is a remote source as a digest holds it: by repository and
// path, without the ref, commit, and h1: that the lock and the record keep,
// and with the hash of its package, so that a commit that leaves the
// package alone, or another ref to the same content, changes no unit.
func digestSource(id *sources.Identity, hash string) map[string]any {
	s := map[string]any{"repository": id.Repository, "path": id.Path}
	if hash != "" {
		s["hash"] = hash
	}
	return s
}

// hashed is a sensitive value as a digest holds it: the HMAC of its
// canonical JSON under a key that the data key of the stack and the id of
// the record give, as Vault's audit log does, so the digest changes when
// the value does and tells nothing about it.
func (e *engine) hashed(inst *instance, v any) (string, error) {
	key, err := e.dataKey()
	if err != nil {
		return "", err
	}
	dk, err := state.DigestKey(key, e.recordID(inst))
	if err != nil {
		return "", err
	}
	data, err := canonical(v)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, dk)
	mac.Write(data)
	return "hmac:v1:" + hex.EncodeToString(mac.Sum(nil)), nil
}

// recordID is the id of the record of inst: the one it has, or for a unit
// without one, the id up gives it, made once a run, so the digests of a
// plan and of up agree.
func (e *engine) recordID(inst *instance) string {
	if inst.record != nil && inst.record.Tombstone == nil {
		return inst.record.ID
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.newIDs == nil {
		e.newIDs = map[string]string{}
	}
	if id, ok := e.newIDs[inst.address]; ok {
		return id
	}
	id := state.NewID()
	e.newIDs[inst.address] = id
	return id
}

// files hashes the files of sources, relative to dir, without those that
// match generates, and lists the generates that are missing: a path that
// is not there, or a pattern that matches no file; and the patterns of
// sources that match no file.
func files(dir string, sources []model.PathItem, generates []string) (map[string]any, []string, []string, error) {
	made := map[string]bool{}
	var missing, empty []string
	for _, g := range generates {
		matches, err := glob(dir, g)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("generates: %w", err)
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
			return nil, nil, nil, fmt.Errorf("sources: %w", err)
		}
		if len(matches) == 0 {
			empty = append(empty, item.Pattern)
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
			return nil, nil, nil, err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = sum
	}
	return out, missing, empty, nil
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
