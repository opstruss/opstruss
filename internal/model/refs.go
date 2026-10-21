package model

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/graph"
)

// Facts are the names under opstruss. that expressions may read.
var Facts = []string{"address", "dir", "cwd", "project", "artifacts", "temp", "state_key", "reason", "os", "arch", "host", "time", "run"}

// planFacts are the facts known when a plan is made, the only ones that if,
// for_each, and locals may read.
var planFacts = []string{"project", "os", "arch", "host"}

// place is where an expression is written, which decides what it may read.
type place struct {
	kind    string // unit, until, cond, local, task, check, plugin, plugin-check, output
	each    bool
	task    *Task
	steps   map[string]bool
	forUnit bool // a task that a unit uses, whose expressions read state_key and reason
}

var readable = map[string][]string{
	"unit":         {"inputs", "locals", "units", "stack", "each", "secrets", "opstruss"},
	"until":        {"inputs", "locals", "units", "stack", "each", "secrets", "opstruss", "outputs"},
	"cond":         {"inputs", "locals", "units", "stack", "each", "opstruss"},
	"local":        {"inputs", "locals", "stack", "secrets", "opstruss"},
	"task":         {"inputs", "locals", "units", "tasks", "steps", "secrets", "stack", "each", "event", "opstruss"},
	"check":        {"inputs", "locals", "units", "tasks", "steps", "secrets", "stack", "each", "event", "opstruss", "outputs"},
	"plugin":       {"inputs", "steps", "each", "opstruss"},
	"plugin-check": {"inputs", "steps", "each", "opstruss", "outputs"},
	"output":       {"inputs", "locals", "units", "stack", "opstruss"},
}

var placeNames = map[string]string{
	"unit": "a unit", "until": "retry.until of a unit", "cond": "if and for_each of a unit", "local": "locals",
	"task": "a task", "check": "failed_when, changed_when, and retry.until", "output": "project outputs",
	"plugin": "a plugin task", "plugin-check": "failed_when, changed_when, and retry.until of a plugin task",
}

// refs checks the expressions in v, written at path, and returns the units
// they read.
func (c *checker) refs(path []string, v any, pl place) []string {
	if v == nil {
		return nil
	}
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		c.add(path, "%v", err)
		return nil
	}
	var units []string
	for _, r := range use.Refs {
		root := r.Path[0]
		if !slices.Contains(readable[pl.kind], root) {
			c.add(path, "%s cannot read %s (in ${{ %s }})", placeNames[pl.kind], root, r.Expr)
			continue
		}
		name := ""
		if len(r.Path) > 1 {
			name = r.Path[1]
		}
		switch root {
		case "inputs":
			if name != "" && !c.hasInput(name, pl) {
				c.add(path, "there is no input %s (in ${{ %s }})", name, r.Expr)
			}
		case "locals":
			if name != "" && !hasKey(c.p.Locals, name) {
				c.add(path, "there is no local %s (in ${{ %s }})", name, r.Expr)
			}
		case "units":
			if name == "" {
				c.add(path, "name the unit, as units.<name> (in ${{ %s }})", r.Expr)
				continue
			}
			u := c.p.Units[name]
			if u == nil {
				c.add(path, "there is no unit %s (in ${{ %s }})", name, r.Expr)
				continue
			}
			units = append(units, name)
			if len(r.Path) > 3 && r.Path[2] == "outputs" && !hasOutput(u, r.Path[3]) {
				c.add(path, "unit %s has no output %s (in ${{ %s }})", name, r.Path[3], r.Expr)
			}
		case "tasks":
			if name != "" && !slices.Contains(pl.task.Needs, name) {
				c.add(path, "tasks.%s is read through needs: [%s] (in ${{ %s }})", name, name, r.Expr)
			}
		case "steps":
			if name != "" && !pl.steps[name] {
				c.add(path, "there is no step %s before this one (in ${{ %s }})", name, r.Expr)
			}
		case "secrets":
			if c.p != nil && c.p.component {
				c.add(path, "a component reads no secrets; give it the value through one of its inputs (in ${{ %s }})", r.Expr)
				continue
			}
			if r.Dynamic && len(r.Path) == 1 {
				c.add(path, "secrets are read by name, as secrets.<name> (in ${{ %s }})", r.Expr)
			}
		case "each":
			if !pl.each {
				c.add(path, "each is only in something with for_each (in ${{ %s }})", r.Expr)
			}
		case "opstruss":
			c.fact(path, name, r, pl)
		}
	}
	return units
}

// hasInput says whether an expression at pl reads an input that exists:
// of its task, or of the project, or of the plugin in a plugin file.
func (c *checker) hasInput(name string, pl place) bool {
	switch {
	case pl.task != nil && pl.task.Inputs[name] != nil:
		return true
	case c.plugin != nil:
		return c.plugin.Inputs[name] != nil
	}
	return c.p.Inputs[name] != nil
}

// checkPlace is where failed_when, changed_when, and retry.until of
// something at pl are, which also read outputs.
func checkPlace(pl place) place {
	pl.kind = map[string]string{"task": "check", "plugin": "plugin-check"}[pl.kind]
	return pl
}

func (c *checker) fact(path []string, name string, r expr.Ref, pl place) {
	switch {
	case name == "":
		c.add(path, "read one fact, such as opstruss.project (in ${{ %s }})", r.Expr)
	case !slices.Contains(Facts, name):
		c.add(path, "opstruss has no fact %s (in ${{ %s }})", name, r.Expr)
	case (pl.kind == "cond" || pl.kind == "local") && !slices.Contains(planFacts, name):
		c.add(path, "%s read only facts known at plan, %s; not opstruss.%s (in ${{ %s }})", placeNames[pl.kind], "opstruss.project, .os, .arch, and .host", name, r.Expr)
	case (pl.kind == "unit" || pl.kind == "until") && slices.Contains([]string{"cwd", "artifacts", "temp", "state_key", "reason"}, name):
		c.add(path, "a unit cannot read opstruss.%s, which changes with each run or place (in ${{ %s }})", name, r.Expr)
	case (pl.kind == "task" || pl.kind == "check") && !pl.forUnit && (name == "state_key" || name == "reason"):
		c.add(path, "opstruss.%s is only in a task that a unit uses (in ${{ %s }})", name, r.Expr)
	}
}

// hasOutput says whether unit u may have output k: a run unit, a unit of
// a plugin, and a unit that uses a component have the outputs they
// declare, a plugin with outputs: tool any, and other units are checked
// when they run.
func hasOutput(u *Unit, k string) bool {
	switch t := u.Target; {
	case u.Run != nil:
		return u.Outputs[k] != nil
	case t != nil && t.Kind == TargetPlugin && !t.Package.Plugin.Outputs.Tool:
		return t.Package.Plugin.Outputs.Declared[k] != nil
	case t != nil && t.Kind == TargetComponent && t.Component != nil:
		return t.Component.Outputs[k] != nil
	}
	return true
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

func envValues(env Env) map[string]any {
	out := map[string]any{}
	for k, v := range env {
		out[k] = v.Text
	}
	return out
}

func argv(cmd *Command) any {
	if cmd == nil {
		return nil
	}
	out := make([]any, len(cmd.Argv))
	for i, a := range cmd.Argv {
		out[i] = a
	}
	return out
}

func requiresValue(r *Requires) any {
	switch {
	case r == nil:
		return nil
	case r.Expr != "":
		return r.Expr
	}
	out := make([]any, len(r.List))
	for i, s := range r.List {
		out[i] = s
	}
	return out
}

func stdinValue(s *Stdin) any {
	if s == nil {
		return nil
	}
	return s.Value
}

// condition returns a condition key as the expression it is, written with
// or without ${{ }}.
func condition(v any) any {
	if s, ok := v.(string); ok && !expr.HasExpr(s) {
		return "${{ " + s + " }}"
	}
	return v
}

func retryUntil(r *Retry) any {
	if r == nil {
		return nil
	}
	return condition(r.Until)
}

// unitValues are the keys of a unit whose expressions read other units.
func unitValues(u *Unit) map[string]any {
	return map[string]any{"with": u.With, "env": envValues(u.Env), "run": argv(u.Run), "service": argv(u.Service),
		"down": argv(u.Down), "dir": u.Dir, "revision": u.Revision, "requires": requiresValue(u.Requires),
		"stdin.value": stdinValue(u.Stdin), "retry.until": retryUntil(u.Retry), "if": condition(u.If), "for_each": u.ForEach}
}

// references checks every expression, and builds the graph of task calls
// and the graph of locals; the graph of units goes through components,
// so the project builds it once every file is checked.
func (c *checker) references() {
	p := c.p
	localDeps := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(p.Locals)) {
		use, err := expr.AnalyzeValue(p.Locals[name])
		c.refs([]string{"locals", name}, p.Locals[name], place{kind: "local"})
		localDeps[name] = nil
		if err != nil {
			continue
		}
		for _, r := range use.Refs {
			if r.Path[0] == "locals" && len(r.Path) > 1 && hasKey(p.Locals, r.Path[1]) {
				localDeps[name] = append(localDeps[name], r.Path[1])
			}
		}
	}
	if _, err := graph.Build("locals", localDeps); err != nil {
		c.add([]string{"locals"}, "%v", err)
	}

	for _, name := range slices.Sorted(maps.Keys(p.Units)) {
		u, path := p.Units[name], []string{"units", name}
		each := u.ForEach != nil
		for key, v := range unitValues(u) {
			pl := place{kind: "unit", each: each}
			switch key {
			case "retry.until":
				pl.kind = "until"
			case "if":
				pl.kind = "cond"
			case "for_each":
				pl = place{kind: "cond"}
			}
			c.refs(at(path, strings.Split(key, ".")...), v, pl)
		}
		for _, tn := range slices.Sorted(maps.Keys(u.Tasks)) {
			c.taskRefs(at(path, "tasks", tn), u.Tasks[tn])
		}
	}

	calls := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		t := p.Tasks[name]
		c.taskRefs([]string{"tasks", name}, t)
		calls[name] = slices.DeleteFunc(slices.Clone(t.Needs), func(n string) bool { return p.Tasks[n] == nil })
		walkSteps(t.Steps, func(s *Step) {
			if p.Tasks[s.Task] != nil {
				calls[name] = append(calls[name], s.Task)
			}
		})
	}
	if _, err := graph.Build("tasks", calls); err != nil {
		c.add([]string{"tasks"}, "%v", err)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Outputs)) {
		c.refs([]string{"outputs", name}, p.Outputs[name].Value, place{kind: "output"})
	}
}

func walkSteps(steps []*Step, fn func(*Step)) {
	for _, s := range steps {
		fn(s)
		walkSteps(s.Block, fn)
		walkSteps(s.Rescue, fn)
		walkSteps(s.Always, fn)
	}
}

func (c *checker) taskRefs(path []string, t *Task) {
	pl := place{kind: "task", each: t.ForEach != nil, task: t, steps: map[string]bool{}, forUnit: c.usedByUnit(path)}
	if c.plugin != nil {
		pl.kind = "plugin"
	}
	check := checkPlace(pl)
	for key, v := range map[string]any{"run": argv(t.Run), "env": envValues(t.Env), "requires": requiresValue(t.Requires),
		"if": condition(t.If), "for_each": t.ForEach} {
		c.refs(at(path, key), v, pl)
	}
	c.refs(at(path, "stdin", "value"), stdinValue(t.Stdin), pl)
	if t.Concurrency != nil {
		c.refs(at(path, "concurrency", "group"), t.Concurrency.Group, pl)
	}
	c.refs(at(path, "failed_when"), condition(t.FailedWhen), check)
	c.refs(at(path, "changed_when"), condition(t.ChangedWhen), check)
	c.refs(at(path, "retry", "until"), retryUntil(t.Retry), check)
	for _, name := range slices.Sorted(maps.Keys(t.Calls)) {
		c.refs(at(path, name), t.Calls[name], pl)
	}
	c.stepRefs(at(path, "steps"), t.Steps, pl)
	for _, name := range slices.Sorted(maps.Keys(t.Outputs)) {
		c.refs(at(path, "outputs", name, "value"), t.Outputs[name].Value, pl)
	}
}

// stepRefs checks steps in order: each step reads the steps before it.
func (c *checker) stepRefs(path []string, steps []*Step, pl place) {
	for i, s := range steps {
		sp := at(path, strconv.Itoa(i))
		sl := pl
		sl.each = pl.each || s.ForEach != nil
		check := checkPlace(sl)
		c.refs(at(sp, "if"), condition(s.If), sl)
		for key, v := range map[string]any{"run": argv(s.Run), "with": s.With, "env": envValues(s.Env)} {
			c.refs(at(sp, key), v, sl)
		}
		units := make([]any, len(s.Units))
		for j, u := range s.Units {
			units[j] = u
		}
		c.refs(at(sp, "units"), units, sl)
		for _, name := range slices.Sorted(maps.Keys(s.Calls)) {
			c.refs(at(sp, name), s.Calls[name], sl)
		}
		c.refs(at(sp, "stdin", "value"), stdinValue(s.Stdin), sl)
		c.refs(at(sp, "for_each"), s.ForEach, pl)
		c.refs(at(sp, "failed_when"), condition(s.FailedWhen), check)
		c.refs(at(sp, "changed_when"), condition(s.ChangedWhen), check)
		c.refs(at(sp, "retry", "until"), retryUntil(s.Retry), check)
		c.stepRefs(at(sp, "block"), s.Block, sl)
		c.stepRefs(at(sp, "rescue"), s.Rescue, sl)
		c.stepRefs(at(sp, "always"), s.Always, sl)
		if c.plugin == nil && s.Task != "" && c.p.Tasks[s.Task] == nil && !slices.Contains(StandardTasks, s.Task) && !c.unitTask(s.Task) {
			c.add(at(sp, "task"), "there is no task %s", s.Task)
		}
		if s.ID != "" {
			pl.steps[s.ID] = true
		}
	}
}

// usedByUnit says whether the task at path is one that a unit uses.
func (c *checker) usedByUnit(path []string) bool {
	if c.p == nil || len(path) != 2 || path[0] != "tasks" {
		return false
	}
	for _, u := range c.p.Units {
		if t := u.Target; t != nil && t.Kind == TargetProject && t.Task == path[1] {
			return true
		}
	}
	return false
}

// unitTaskCycle names a step that runs plan, up, or down of units in the
// task name or a task it calls, which a unit that uses name cannot run
// at its turn.
func (c *checker) unitTaskCycle(name string, seen map[string]bool) string {
	if seen[name] || c.p.Tasks[name] == nil {
		return ""
	}
	seen[name] = true
	t := c.p.Tasks[name]
	found := ""
	walkSteps(t.Steps, func(s *Step) {
		switch {
		case found != "":
		case slices.Contains([]string{"plan", "up", "down"}, s.Task):
			found = "task " + name + " runs task: " + s.Task
		case s.Task != "":
			found = c.unitTaskCycle(s.Task, seen)
		}
	})
	for _, n := range t.Needs {
		if found == "" {
			found = c.unitTaskCycle(n, seen)
		}
	}
	return found
}

// unitTask says whether a unit of these files, or of the components they
// use, has the task name; an instance of a component has the tasks of the
// component that are not internal.
func (c *checker) unitTask(name string) bool { return hasUnitTask(c.p, name, map[*Project]bool{}) }

func hasUnitTask(p *Project, name string, seen map[*Project]bool) bool {
	if seen[p] {
		return false
	}
	seen[p] = true
	for _, u := range p.Units {
		if u.Tasks[name] != nil {
			return true
		}
		if t := u.Target; t != nil && t.Component != nil {
			if ct := t.Component.Tasks[name]; ct != nil && !ct.Internal || hasUnitTask(t.Component, name, seen) {
				return true
			}
		}
	}
	return false
}

// EvalLocals evaluates the locals with the project inputs, and kv() through
// kv. A local that reads what is only known in a run, a secret, or kv()
// without kv, stays as written.
func (p *Project) EvalLocals(inputs map[string]any, kv func(string) (any, error)) (map[string]any, error) {
	return p.evalLocals(p.Name, inputs, kv, false)
}

// LocalMarks says which locals are sensitive, given the marks of what they
// read, such as {"inputs": {"token": true}}, and which names of kv() hold
// sensitive values, when kvMarked is not nil: a local that reads a
// sensitive value, or a sensitive local, is sensitive as a whole.
func (p *Project) LocalMarks(marks map[string]any, kvMarked func(string) bool) map[string]any {
	out := map[string]any{}
	if len(marks) == 0 && kvMarked == nil {
		return out
	}
	scope := maps.Clone(marks)
	scope["locals"] = out
	// Locals read locals, so this goes on until no local changes.
	for changed := true; changed; {
		changed = false
		for name, v := range p.Locals {
			if out[name] != nil {
				continue
			}
			use, err := expr.AnalyzeValue(v)
			if err != nil {
				continue
			}
			for _, r := range use.Refs {
				if expr.MarkAt(scope, r.Path...) != nil {
					out[name], changed = true, true
					break
				}
			}
			for _, k := range use.KV {
				if out[name] == nil && kvMarked != nil && kvMarked(k) {
					out[name], changed = true, true
				}
			}
		}
	}
	return out
}

// ComponentLocals evaluates the locals of a component with its inputs, in
// the project named project. A local that fails, as one that reads an
// input with no value yet, fails only the expressions that read it.
func (p *Project) ComponentLocals(project string, inputs map[string]any, kv func(string) (any, error)) map[string]any {
	out, err := p.evalLocals(project, inputs, kv, true)
	if err != nil {
		return map[string]any{}
	}
	return out
}

func (p *Project) evalLocals(project string, inputs map[string]any, kv func(string) (any, error), lenient bool) (map[string]any, error) {
	deps := map[string][]string{}
	uses := map[string]*expr.Use{}
	for name, v := range p.Locals {
		use, err := expr.AnalyzeValue(v)
		if err != nil {
			return nil, err
		}
		uses[name] = use
		deps[name] = nil
		for _, r := range use.Refs {
			if r.Path[0] == "locals" && len(r.Path) > 1 {
				deps[name] = append(deps[name], r.Path[1])
			}
		}
	}
	g, err := graph.Build("locals", deps)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	deferred := map[string]bool{}
	readsKV := map[string]bool{}
	for _, name := range g.Order {
		use := uses[name]
		readsKV[name] = slices.Contains(use.Funcs, "kv")
		later := false
		for _, r := range use.Refs {
			if r.Path[0] == "locals" && len(r.Path) > 1 {
				readsKV[name] = readsKV[name] || readsKV[r.Path[1]]
				later = later || deferred[r.Path[1]]
			}
			later = later || r.Path[0] == "secrets"
		}
		if later || kv == nil && readsKV[name] {
			deferred[name] = true
			out[name] = p.Locals[name]
			continue
		}
		s := &expr.Scope{Dir: p.DirOf("locals." + name), KV: kv, Vars: map[string]any{
			"inputs": inputs, "locals": out, "stack": p.StackName,
			"opstruss": map[string]any{"project": project, "os": runtime.GOOS, "arch": runtime.GOARCH, "host": hostname()},
		}}
		v, err := expr.EvalValue(p.Locals[name], s)
		switch {
		case err != nil && (readsKV[name] || lenient):
			// What kv() reads changes after the files are checked, as the
			// inputs of a component do during a run, so a local that fails
			// on it fails what reads the local instead.
			out[name] = expr.Broken(fmt.Errorf("local %s: %w", name, err))
		case err != nil:
			return nil, fmt.Errorf("local %s: %w", name, err)
		default:
			out[name] = v
		}
	}
	return out, nil
}

// DirOf returns the directory of the file that wrote an entry, which
// relative paths and file() read from.
func (p *Project) DirOf(key string) string {
	if o, ok := p.origins[key]; ok {
		return filepath.Join(p.base, filepath.Dir(o.doc.file))
	}
	return p.Dir
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
