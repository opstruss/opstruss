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

// planFacts are the facts known when a plan is made, the only ones that if,
// for_each, and locals may read.
var planFacts = []string{"project", "os", "arch", "host"}

// EventFields are the fields of the event of each trigger beside time and
// trigger, as its source gives one event.
var EventFields = map[string][]string{
	"schedule": nil,
	"every":    nil,
	"webhook":  {"method", "path", "headers", "query", "body"},
	"watch":    {"unit", "stream", "line", "paths"},
	"after":    {"task", "unit", "outcome", "outputs"},
}

// triggerEvent is what event holds in a run of trigger kind, w for a
// watch: what its source gives, also data, the JSON of a line that a call
// or the hook of a plugin writes, and count of paths, and what a batch or
// debounce gathers.
func triggerEvent(kind string, w *Watch) []string {
	fields := append([]string{"time", "trigger"}, EventFields[kind]...)
	if kind == "watch" {
		fields = append(fields, "data", "count")
		if w.Batch != "" || w.Debounce != "" {
			fields = append(fields, "lines", "truncated")
		}
	}
	return fields
}

// andList writes words as a sentence lists them: a, b, and c.
func andList(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
}

// taskEvent is what event may hold in a run of task t, in a run of any
// of its triggers, or nil for a task without on:, which --event calls with
// any trigger.
func taskEvent(t *Task) []string {
	if t.On == nil {
		return nil
	}
	fields := []string{"time", "trigger"}
	if len(t.On.Webhook) > 0 {
		fields = append(fields, triggerEvent("webhook", nil)...)
	}
	if len(t.On.After) > 0 {
		fields = append(fields, triggerEvent("after", nil)...)
	}
	for _, w := range t.On.Watch {
		fields = append(fields, triggerEvent("watch", w)...)
	}
	slices.Sort(fields)
	return slices.Compact(fields)
}

// place is where an expression is written, which decides what it may read.
type place struct {
	kind    string // unit, until, cond, local, task, check, plugin, plugin-check, output, secret, trigger
	each    bool
	task    *Task
	steps   map[string]*Step // the steps before, by id
	forUnit bool             // a task that a unit uses, whose expressions read state_key and reason
	event   []string         // the fields of event there, or nil when any may be
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
	"secret":       {"inputs", "locals", "stack", "opstruss"},
	"trigger":      {"inputs", "locals", "units", "secrets", "stack", "event", "opstruss"},
	"install":      {"inputs", "locals", "units", "secrets", "stack", "opstruss"},
}

var placeNames = map[string]string{
	"unit": "a unit", "until": "retry.until of a unit", "cond": "if and for_each of a unit", "local": "locals",
	"task": "a task", "check": "failed_when, changed_when, and retry.until", "output": "project outputs",
	"plugin": "a plugin task", "plugin-check": "failed_when, changed_when, and retry.until of a plugin task",
	"secret": "secret sources", "trigger": "if and with of a trigger", "install": "what the daemon reads as it installs a trigger",
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
			trigger := pl.kind == "trigger" || pl.kind == "install"
			switch {
			case name != "" && !hasKey(c.p.Locals, name):
				c.add(path, "there is no local %s (in ${{ %s }})", name, r.Expr)
			case trigger && name != "" && c.programLocal(name):
				c.add(path, "a trigger reads only the secrets that sops and env hold, and local %s reads one that a program gives, which runs only in a run (in ${{ %s }})", name, r.Expr)
			case trigger && name == "" && c.programLocal(""):
				c.add(path, "a trigger reads locals by name, as locals.<name>, since a local reads a secret that a program gives (in ${{ %s }})", r.Expr)
			case slices.Contains(readable[pl.kind], "secrets"):
				// What reads secrets reads the locals that do as well.
			case name != "" && c.secretLocal(name):
				c.add(path, "%s cannot read local %s, which reads a secret (in ${{ %s }})", placeNames[pl.kind], name, r.Expr)
			case name == "" && c.secretLocal(""):
				c.add(path, "%s read locals by name, as locals.<name>, since a local reads a secret (in ${{ %s }})", placeNames[pl.kind], r.Expr)
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
			if name == "" {
				continue
			}
			s := pl.steps[name]
			if s == nil {
				c.add(path, "there is no step %s before this one (in ${{ %s }})", name, r.Expr)
				continue
			}
			if msg := c.stepField(s, r.Path[2:]); msg != "" {
				c.add(path, "%s (in ${{ %s }})", msg, r.Expr)
			}
		case "secrets":
			if c.p != nil && c.p.component {
				c.add(path, "a component reads no secrets; give it the value through one of its inputs (in ${{ %s }})", r.Expr)
				continue
			}
			switch {
			case len(r.Path) == 1:
				c.add(path, "secrets are read by name, as secrets.<name> (in ${{ %s }})", r.Expr)
			case name != "" && c.p.Secrets[name] == nil:
				c.add(path, "there is no secret %s (in ${{ %s }})", name, r.Expr)
			case (pl.kind == "trigger" || pl.kind == "install") && fromProgram(c.p.Secrets[name]):
				c.add(path, "a trigger reads only the secrets that sops and env hold, and %s comes from a program, which runs only in a run (in ${{ %s }})", name, r.Expr)
			}
		case "each":
			if !pl.each {
				c.add(path, "each is only in something with for_each (in ${{ %s }})", r.Expr)
			}
		case "event":
			if name != "" && pl.event != nil && !slices.Contains(pl.event, name) {
				who := "the triggers of this task give"
				if pl.kind == "trigger" {
					who = "this trigger gives"
				}
				c.add(path, "%s no event.%s; event has %s%s (in ${{ %s }})", who, name, andList(pl.event),
					expr.DidYouMean(name, pl.event), r.Expr)
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

// fromProgram says whether secret s comes from a program or a task, which
// only a run starts.
func fromProgram(s *Secret) bool { return s.Run != nil || len(s.Calls) > 0 }

// programLocal says, as secretLocal does, whether local name reads a
// secret that comes from a program or a task, or for "", whether a local
// does.
func (c *checker) programLocal(name string) bool {
	if c.readsProgram == nil {
		names := map[string]any{}
		for n, s := range c.p.Secrets {
			if fromProgram(s) {
				names[n] = true
			}
		}
		c.readsProgram = map[string]any{}
		if len(names) > 0 {
			c.readsProgram = c.p.LocalMarks(map[string]any{"secrets": names}, nil)
		}
	}
	if name == "" {
		return len(c.readsProgram) > 0
	}
	return c.readsProgram[name] != nil
}

// secretLocal says whether local name reads a secret, itself or through
// other locals, or for "", whether a local does: only what may read
// secrets may read it.
func (c *checker) secretLocal(name string) bool {
	if c.readsSecret == nil {
		c.readsSecret = c.p.LocalMarks(map[string]any{"secrets": true}, nil)
	}
	if name == "" {
		return len(c.readsSecret) > 0
	}
	return c.readsSecret[name] != nil
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
	case !slices.Contains(expr.Facts, name):
		c.add(path, "opstruss has no fact %s (in ${{ %s }})", name, r.Expr)
	case (pl.kind == "cond" || pl.kind == "local" || pl.kind == "secret") && !slices.Contains(planFacts, name):
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

// Value is r as written: its expression, or its list.
func (r *Requires) Value() any {
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
	v := map[string]any{"with": u.With, "env": envValues(u.Env), "run": argv(u.Run), "service": argv(u.Service),
		"destroy": argv(u.Destroy), "dir": u.Dir, "revision": u.Revision, "requires": u.Requires.Value(),
		"stdin.value": stdinValue(u.Stdin), "retry.until": retryUntil(u.Retry), "if": condition(u.If), "for_each": u.ForEach}
	if r := u.Ready; r != nil {
		v["ready.http"], v["ready.tcp"], v["ready.run"] = r.HTTP, r.TCP, argv(r.Run)
	}
	if u.Stop != nil {
		v["stop.run"] = argv(u.Stop.Run)
	}
	return v
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
		c.addEach([]string{"locals"}, err)
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
		if t.On != nil {
			for i, s := range t.On.Schedule {
				sp := []string{"tasks", name, "on", "schedule"}
				if len(t.On.Schedule) > 1 {
					sp = at(sp, strconv.Itoa(i))
				}
				pl := place{kind: "trigger", event: triggerEvent("schedule", nil)}
				c.refs(at(sp, "if"), condition(s.If), pl)
				c.refs(at(sp, "with"), s.With, pl)
			}
			for i, w := range t.On.Watch {
				wp := []string{"tasks", name, "on", "watch"}
				if len(t.On.Watch) > 1 {
					wp = at(wp, strconv.Itoa(i))
				}
				for call, args := range w.Calls {
					c.refs(at(wp, call), args, place{kind: "install"})
				}
				for j, it := range w.Paths {
					c.refs(at(wp, "paths", strconv.Itoa(j)), it.Pattern, place{kind: "install"})
				}
				pl := place{kind: "trigger", event: triggerEvent("watch", w)}
				c.refs(at(wp, "if"), condition(w.If), pl)
				c.refs(at(wp, "with"), w.With, pl)
			}
			for i, a := range t.On.After {
				ap := []string{"tasks", name, "on", "after"}
				if len(t.On.After) > 1 {
					ap = at(ap, strconv.Itoa(i))
				}
				pl := place{kind: "trigger", event: triggerEvent("after", nil)}
				c.refs(at(ap, "if"), condition(a.If), pl)
				c.refs(at(ap, "with"), a.With, pl)
			}
			for i, w := range t.On.Webhook {
				wp := []string{"tasks", name, "on", "webhook"}
				if len(t.On.Webhook) > 1 {
					wp = at(wp, strconv.Itoa(i))
				}
				c.refs(at(wp, "secret"), w.Secret, place{kind: "install"})
				pl := place{kind: "trigger", event: triggerEvent("webhook", nil)}
				c.refs(at(wp, "if"), condition(w.If), pl)
				c.refs(at(wp, "with"), w.With, pl)
			}
		}
		calls[name] = slices.DeleteFunc(slices.Clone(t.Needs), func(n string) bool { return p.Tasks[n] == nil })
		walkSteps(t.Steps, func(s *Step) {
			if p.Tasks[s.Task] != nil {
				calls[name] = append(calls[name], s.Task)
			}
		})
	}
	if _, err := graph.Build("tasks", calls); err != nil {
		c.addEach([]string{"tasks"}, err)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Outputs)) {
		c.refs([]string{"outputs", name}, p.Outputs[name].Value, place{kind: "output"})
	}
	for _, name := range slices.Sorted(maps.Keys(p.Secrets)) {
		s, path := p.Secrets[name], []string{"secrets", name}
		values := map[string]any{"sops": s.Sops, "field": s.Field, "env": envValues(s.Env), "run": argv(s.Run), "dir": s.Dir}
		for _, key := range slices.Sorted(maps.Keys(values)) {
			c.refs(at(path, key), values[key], place{kind: "secret"})
		}
		for _, callee := range slices.Sorted(maps.Keys(s.Calls)) {
			c.refs(at(path, callee), s.Calls[callee], place{kind: "secret"})
		}
	}
}

// CallNames are the names of the tasks that t calls by name, itself or in
// its steps.
func CallNames(t *Task) []string {
	names := slices.Collect(maps.Keys(t.Calls))
	walkSteps(t.Steps, func(s *Step) {
		for name := range s.Calls {
			names = append(names, name)
		}
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// PassesOf are the pass: patterns of t and of its steps.
func PassesOf(t *Task) []string {
	passes := slices.Clone(t.Pass)
	walkSteps(t.Steps, func(s *Step) { passes = append(passes, s.Pass...) })
	return passes
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
	pl := place{kind: "task", each: t.ForEach != nil, task: t, steps: map[string]*Step{}, forUnit: c.usedByUnit(path), event: taskEvent(t)}
	if c.plugin != nil {
		pl.kind = "plugin"
	}
	check := checkPlace(pl)
	for key, v := range map[string]any{"run": argv(t.Run), "env": envValues(t.Env), "requires": t.Requires.Value(),
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
		// rescue: and always: read the outcome of their own block.
		if s.ID != "" && len(s.Block) > 0 {
			pl.steps[s.ID] = s
		}
		c.stepRefs(at(sp, "rescue"), s.Rescue, sl)
		c.stepRefs(at(sp, "always"), s.Always, sl)
		if c.plugin == nil && s.Task != "" && c.p.Tasks[s.Task] == nil && !slices.Contains(StandardTasks, s.Task) && !c.unitTask(s.Task) {
			c.add(at(sp, "task"), "there is no task %s", s.Task)
		}
		if s.ID != "" {
			pl.steps[s.ID] = s
		}
	}
}

// stepFields are what steps.<id> gives.
var stepFields = []string{"outcome", "changed", "outputs", "units"}

// stepField says what is wrong with reading rest after steps.<id> of s,
// such as a field that a step does not give or an output it does not
// have, or "". A step with for_each, or that calls a task with it, gives
// one result per key, which is not checked.
func (c *checker) stepField(s *Step, rest []string) string {
	if len(rest) == 0 || c.perKey(s) {
		return ""
	}
	field := rest[0]
	if !slices.Contains(stepFields, field) {
		msg := fmt.Sprintf("step %s has no field %s; a step has outcome, changed, outputs, and units", s.ID, field)
		switch {
		case expr.IsFunction(field):
			msg += fmt.Sprintf("; %s is a function: %s(steps.%s)", field, field, s.ID)
		case expr.Nearest(field, stepFields) != "":
			msg += "; did you mean " + expr.Nearest(field, stepFields)
		case s.Run != nil:
			msg += fmt.Sprintf("; what a run step prints is steps.%s.outputs.stdout", s.ID)
		}
		return msg
	}
	if field != "outputs" || len(rest) < 2 {
		return ""
	}
	outs, known := c.stepOutputs(s)
	if !known || slices.Contains(outs, rest[1]) {
		return ""
	}
	name := rest[1]
	msg := fmt.Sprintf("step %s has no output %s", s.ID, name)
	if len(outs) == 0 {
		msg += "; it gives none"
	} else {
		msg += "; it gives " + strings.Join(outs, ", ")
	}
	switch {
	case expr.IsFunction(name):
		msg += fmt.Sprintf("; %s is a function: %s(steps.%s.outputs)", name, name, s.ID)
	case expr.Nearest(name, outs) != "":
		msg += "; did you mean " + expr.Nearest(name, outs)
	}
	return msg
}

// perKey says whether s gives one result per key: it has for_each, or it
// calls a task of the project that has it.
func (c *checker) perKey(s *Step) bool {
	if s.ForEach != nil {
		return true
	}
	if s.Task != "" && c.p != nil {
		if t := c.p.Tasks[s.Task]; t != nil {
			return t.ForEach != nil
		}
	}
	return false
}

// stepOutputs are the names of the outputs that s gives, and false when
// they are known only when it runs.
func (c *checker) stepOutputs(s *Step) ([]string, bool) {
	switch {
	case s.Run != nil:
		return append([]string{"exit_code", "stderr", "stdout"}, slices.Sorted(maps.Keys(s.Outputs))...), true
	case s.Task != "" && slices.Contains(StandardTasks, s.Task):
		// What a pass gives is per unit, in units.
		return nil, true
	case s.Task != "":
		if c.p == nil || c.plugin != nil || c.p.Tasks[s.Task] == nil {
			return nil, false
		}
		return slices.Sorted(maps.Keys(c.p.Tasks[s.Task].Outputs)), true
	}
	for name, args := range s.Calls {
		switch {
		case name == "set":
			// set gives its inputs as its outputs.
			return slices.Sorted(maps.Keys(args)), true
		case slices.Contains(BuiltinTasks, name):
			return slices.Sorted(maps.Keys(Builtin(name).Outputs)), true
		case c.p != nil && c.plugin == nil:
			t, err := c.p.Resolve(name)
			if err != nil || t.Kind != TargetTask {
				return nil, false
			}
			pt := t.Package.Plugin.Tasks[t.Task]
			if pt == nil || pt.ForEach != nil {
				return nil, false
			}
			return slices.Sorted(maps.Keys(pt.Outputs)), true
		}
	}
	return nil, false
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

// unitTaskCycle names a step that runs plan, up, or destroy of units in the
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
		case slices.Contains([]string{"plan", "up", "destroy"}, s.Task):
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

// EvalLocals evaluates the locals with the project inputs, kv() through
// kv, and secrets.<name> through secrets, whose values Later makes. A
// local that reads a value Later makes is one itself, made when an
// expression reads it; without secrets, a local that reads one, or kv()
// without kv, stays as written.
func (p *Project) EvalLocals(inputs map[string]any, kv func(string) (any, error), secrets map[string]any) (map[string]any, error) {
	return p.evalLocals(p.Name, inputs, kv, secrets, false)
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
	out, err := p.evalLocals(project, inputs, kv, nil, true)
	if err != nil {
		return map[string]any{}
	}
	return out
}

func (p *Project) evalLocals(project string, inputs map[string]any, kv func(string) (any, error), secrets map[string]any, lenient bool) (map[string]any, error) {
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
			later = later || r.Path[0] == "secrets" && secrets == nil
		}
		if later || kv == nil && readsKV[name] {
			deferred[name] = true
			out[name] = p.Locals[name]
			continue
		}
		s := &expr.Scope{Dir: p.DirOf("locals." + name), KV: kv, Vars: map[string]any{
			"inputs": inputs, "locals": out, "stack": p.StackName, "secrets": secrets,
			"opstruss": map[string]any{"project": project, "os": runtime.GOOS, "arch": runtime.GOARCH, "host": hostname()},
		}}
		if expr.ReadsLater(p.Locals[name], s) {
			// The locals that come after go in out too, which s reads.
			src := p.Locals[name]
			out[name] = expr.Later(func() (any, error) {
				v, err := expr.EvalValue(src, s)
				if err != nil {
					return nil, fmt.Errorf("local %s: %w", name, err)
				}
				return v, nil
			})
			continue
		}
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
