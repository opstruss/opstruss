package model

import (
	"cmp"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/proc"
)

// BuiltinTasks are the tasks in the binary, also called opstruss.<name>.
var BuiltinTasks = []string{"command", "http", "read", "write", "set", "assert", "wait", "log", "kv"}

// StandardTasks are the lifecycle tasks of units.
var StandardTasks = []string{"plan", "up", "down", "status", "logs"}

// Hooks are plugin tasks that only the engine calls.
var Hooks = []string{"watch", "exec", "migrate"}

// Commands are the built-in commands and their aliases, which tasks cannot
// be named.
var Commands = []string{"init", "validate", "explain", "doctor", "list", "outputs", "stop", "mcp", "plugins",
	"unlock", "state", "runs", "run", "exec", "version", "help", "apply", "destroy", "preview", "ps"}

// ansible maps keywords of Ansible to what OpsTruss writes instead.
var ansible = map[string]string{
	"when": "if", "register": "id, then steps.<id>.outputs", "loop": "for_each", "with_items": "for_each",
	"ignore_errors": "continue_on_error", "environment": "env", "until": "retry.until", "retries": "retry.attempts",
	"delay": "retry.delay", "vars": "with", "args": "with", "become": "", "notify": "", "tags": "", "delegate_to": "",
}

var (
	gitSource = regexp.MustCompile(`^github\.com/opstruss/[a-z0-9._-]+(/[A-Za-z0-9._/-]+)?@[A-Za-z0-9._/-]+$`)
	address   = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\[[a-z0-9._-]+\])?(/[a-z][a-z0-9_-]*(\[[a-z0-9._-]+\])?)*$`)
)

// keywords are the keys of steps, tasks, watch sources, and secret
// sources, which names in uses: cannot take.
var keywords = func() map[string]bool {
	k := map[string]bool{"env": true}
	for _, t := range []reflect.Type{reflect.TypeFor[Step](), reflect.TypeFor[Task](), reflect.TypeFor[Watch](), reflect.TypeFor[Secret]()} {
		maps.Copy(k, jsonKeys(t))
	}
	return k
}()

type checker struct {
	p     *Project
	probs Problems
}

func (c *checker) add(path []string, format string, args ...any) {
	c.probs = append(c.probs, c.p.problem(path, format, args...))
}

func at(path []string, more ...string) []string { return append(slices.Clone(path), more...) }

func (p *Project) check() Problems {
	c := &checker{p: p}
	c.root()
	c.uses()
	c.stacks()
	c.units()
	c.tasks()
	c.moves()
	c.references()
	c.unsupported()
	slices.SortStableFunc(c.probs, func(a, b Problem) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Column, b.Column))
	})
	return c.probs
}

func (c *checker) root() {
	if c.p.Name == "" {
		c.add([]string{"name"}, "a project needs a name")
	}
	if c.p.Level != "" && newerLevel(c.p.Level, Level) {
		c.add([]string{"opstruss"}, "this file is for OpsTruss %s or newer; this OpsTruss reads files up to %s", c.p.Level, Level)
	}
}

func newerLevel(a, b string) bool {
	parse := func(s string) (int, int) {
		maj, mn, _ := strings.Cut(s, ".")
		x, _ := strconv.Atoi(maj)
		y, _ := strconv.Atoi(mn)
		return x, y
	}
	am, an := parse(a)
	bm, bn := parse(b)
	return am > bm || am == bm && an > bn
}

func (c *checker) uses() {
	for _, name := range slices.Sorted(maps.Keys(c.p.Uses)) {
		path := []string{"uses", name}
		switch {
		case slices.Contains(BuiltinTasks, name):
			c.add(path, "%s is a built-in task; give the declaration another name", name)
		case keywords[name]:
			c.add(path, "%s is a keyword; give the declaration another name", name)
		case name == "opstruss":
			c.add(path, "opstruss is reserved")
		case strings.HasPrefix(name, "x-"):
			c.add(path, "names in uses: do not start with x-")
		}
		c.environment(path, c.p.Uses[name].Env, c.p.Uses[name].Pass)
		src := c.p.Uses[name].Source
		switch {
		case strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../"):
		case strings.HasPrefix(src, "opstruss."):
			if !slices.Contains(BuiltinTasks, strings.TrimPrefix(src, "opstruss.")) {
				c.add(path, "there is no built-in task %s", strings.TrimPrefix(src, "opstruss."))
			}
		case strings.HasPrefix(src, "github.com/"):
			if !gitSource.MatchString(src) {
				c.add(path, "a git source is github.com/opstruss/<repo>[/<path>]@<ref>")
			}
		default:
			c.add(path, "a source is ./<path>, ../<path>, github.com/opstruss/<repo>@<ref>, or opstruss.<task>")
		}
	}
}

func (c *checker) stacks() {
	for _, s := range slices.Sorted(maps.Keys(c.p.Stacks)) {
		for _, k := range slices.Sorted(maps.Keys(c.p.Stacks[s].With)) {
			if c.p.Inputs[k] == nil {
				c.add([]string{"stacks", s, "with", k}, "the project has no input %s", k)
			}
		}
	}
}

// hasExpr reports a ${{ }} in a script, where values would become shell
// code; $${{ is text.
func hasExpr(cmd *Command) bool {
	if cmd == nil || cmd.Script == "" {
		return false
	}
	return strings.Contains(strings.ReplaceAll(cmd.Script, "$${{", ""), "${{")
}

const scriptExpr = "${{ }} is not allowed in a script; pass values through env:, or write the program as a list"

// environment rejects env: and pass: on the variables of the program
// contract.
func (c *checker) environment(path []string, env Env, pass []string) {
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if ReservedEnv(k) {
			c.add(at(path, "env", k), "OPSTRUSS_ and INPUT_ variables come from OpsTruss; env: cannot set them")
		}
	}
	for i, p := range pass {
		if ReservedEnv(p) {
			c.add(at(path, "pass", strconv.Itoa(i)), "OPSTRUSS_ and INPUT_ variables come from OpsTruss; pass: never passes them")
		}
	}
}

func (c *checker) stop(path []string, s *Stop) {
	if s == nil || s.Signal == "" {
		return
	}
	if _, err := proc.ParseSignal(s.Signal); err != nil {
		c.add(at(path, "stop", "signal"), "%v", err)
	}
}

func (c *checker) units() {
	for _, name := range slices.Sorted(maps.Keys(c.p.Units)) {
		u, path := c.p.Units[name], []string{"units", name}
		kinds := 0
		for _, set := range []bool{u.Run != nil, u.Service != nil, u.Uses != ""} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			c.add(path, "a unit has exactly one of run, service, or uses")
		}
		if u.Service == nil {
			if u.Restart != "" {
				c.add(at(path, "restart"), "restart is for service units")
			}
			if u.Ready != nil {
				c.add(at(path, "ready"), "ready is for service units")
			}
			if u.Stop != nil && u.Stop.Run != nil {
				c.add(at(path, "stop", "run"), "stop.run is for service units")
			}
		} else {
			if u.Down != nil {
				c.add(at(path, "down"), "a service stops through stop:, not down")
			}
			if len(u.Generates) > 0 {
				c.add(at(path, "generates"), "generates is for run units and units that use a task")
			}
		}
		if len(u.Outputs) > 0 && u.Run == nil {
			c.add(at(path, "outputs"), "outputs: is for run units; other units get their outputs from what they run")
		}
		for i, n := range u.Needs {
			if c.p.Units[n] == nil {
				c.add(at(path, "needs", strconv.Itoa(i)), "there is no unit %s", n)
			}
		}
		for k, cmd := range map[string]*Command{"run": u.Run, "service": u.Service, "down": u.Down} {
			if hasExpr(cmd) {
				c.add(at(path, k), scriptExpr)
			}
		}
		c.environment(path, u.Env, u.Pass)
		c.stop(path, u.Stop)
		for _, tn := range slices.Sorted(maps.Keys(u.Tasks)) {
			tp := at(path, "tasks", tn)
			if slices.Contains(StandardTasks, tn) || slices.Contains(Hooks, tn) {
				c.add(tp, "%s is a standard task or hook; unit tasks add tasks and do not replace them", tn)
			}
			c.task(tp, u.Tasks[tn], false)
		}
	}
}

func (c *checker) tasks() {
	webhooks := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.p.Tasks)) {
		t, path := c.p.Tasks[name], []string{"tasks", name}
		if slices.Contains(Commands, name) || slices.Contains(StandardTasks, name) {
			c.add(path, "%s is a built-in command or a standard task; give the task another name", name)
		}
		c.task(path, t, true)
		if t.On == nil {
			continue
		}
		for i, w := range t.On.Webhook {
			wp := at(path, "on", "webhook")
			if len(t.On.Webhook) > 1 {
				wp = at(wp, strconv.Itoa(i))
			}
			if strings.HasPrefix(w.Path, "/-/") {
				c.add(at(wp, "path"), "paths under /-/ belong to OpsTruss")
			}
			key := cmp.Or(w.Method, "POST") + " " + w.Path
			if other, dup := webhooks[key]; dup {
				c.add(at(wp, "path"), "%s is also the webhook of task %s", key, other)
			}
			webhooks[key] = name
		}
	}
}

func callNames(calls Calls) string {
	return strings.Join(slices.Sorted(maps.Keys(calls)), " and ")
}

func (c *checker) task(path []string, t *Task, project bool) {
	kinds := len(t.Calls)
	if t.Run != nil {
		kinds++
	}
	if len(t.Steps) > 0 {
		kinds++
	}
	c.hints(path, t.Calls)
	switch {
	case len(t.Calls) > 1:
		c.add(path, "a task calls one task; found %s", callNames(t.Calls))
	case kinds != 1:
		c.add(path, "a task has exactly one of run, steps, or one call of a task")
	}
	if len(t.Calls) == 1 && len(t.Outputs) > 0 {
		c.add(at(path, "outputs"), "a task that calls one task passes its outputs on; write steps to shape them")
	}
	if hasExpr(t.Run) {
		c.add(at(path, "run"), scriptExpr)
	}
	c.environment(path, t.Env, t.Pass)
	c.stop(path, t.Stop)
	vars := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(t.Inputs)) {
		v := InputEnv(name)
		if other, dup := vars[v]; dup {
			c.add(at(path, "inputs", name), "inputs %s and %s are both %s for the program; rename one", other, name, v)
		}
		vars[v] = name
	}
	for _, name := range slices.Sorted(maps.Keys(t.Outputs)) {
		o := t.Outputs[name]
		switch {
		case len(t.Steps) > 0 && o.Value == nil:
			c.add(at(path, "outputs", name), "an output of a task with steps has value:")
		case len(t.Steps) == 0 && o.Value != nil:
			c.add(at(path, "outputs", name, "value"), "value: is for tasks with steps; other tasks read outputs from their program")
		}
	}
	for i, n := range t.Needs {
		np := at(path, "needs", strconv.Itoa(i))
		dep := c.p.Tasks[n]
		if dep == nil {
			c.add(np, "there is no task %s", n)
			continue
		}
		for _, in := range slices.Sorted(maps.Keys(dep.Inputs)) {
			if dep.Inputs[in].Required && dep.Inputs[in].Default == nil && dep.Inputs[in].Env == "" {
				c.add(np, "task %s needs input %s, and needs passes no inputs; call it with task: and with:", n, in)
			}
		}
	}
	if t.On != nil && !project {
		c.add(at(path, "on"), "on: is only in project tasks")
	}
	c.steps(at(path, "steps"), t.Steps, map[string]bool{})
}

func (c *checker) hints(path []string, calls Calls) {
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if hint, ok := ansibleHint(name); ok {
			c.add(at(path, name), "%s", hint)
		}
	}
}

func ansibleHint(key string) (string, bool) {
	instead, ok := ansible[key]
	switch {
	case !ok:
		return "", false
	case instead == "":
		return key + " is an Ansible keyword, and OpsTruss has none like it", true
	}
	return key + " is an Ansible keyword; OpsTruss writes " + instead, true
}

func (c *checker) steps(path []string, steps []*Step, ids map[string]bool) {
	for i, s := range steps {
		sp := at(path, strconv.Itoa(i))
		kinds := 0
		for _, set := range []bool{s.Run != nil, s.Task != "", len(s.Block) > 0, len(s.Calls) > 0} {
			if set {
				kinds++
			}
		}
		c.hints(sp, s.Calls)
		switch {
		case len(s.Calls) > 1:
			c.add(sp, "a step calls one task; found %s", callNames(s.Calls))
		case kinds != 1:
			c.add(sp, "a step has exactly one of run, task, block, or one call of a task")
		}
		if (len(s.Rescue) > 0 || len(s.Always) > 0) && len(s.Block) == 0 {
			c.add(sp, "rescue and always go with block")
		}
		if s.Task == "" {
			for k, set := range map[string]bool{"with": s.With != nil, "units": len(s.Units) > 0, "yes": s.Yes, "force": s.Force} {
				if set {
					c.add(at(sp, k), "%s is for task: steps", k)
				}
			}
		} else {
			for k, set := range map[string]bool{"env": len(s.Env) > 0, "pass": len(s.Pass) > 0, "shell": s.Shell != "", "dir": s.Dir != "", "stdin": s.Stdin != nil} {
				if set {
					c.add(at(sp, k), "a task: step passes data through with:; the task keeps its own %s", k)
				}
			}
		}
		if len(s.Outputs) > 0 && s.Run == nil {
			c.add(at(sp, "outputs"), "outputs: is for run steps")
		}
		if len(s.Block) > 0 && (s.ForEach != nil || s.Retry != nil) {
			c.add(sp, "a block has no for_each or retry")
		}
		if s.ID != "" {
			if ids[s.ID] {
				c.add(at(sp, "id"), "step id %s is used twice in this task", s.ID)
			}
			ids[s.ID] = true
		}
		if hasExpr(s.Run) {
			c.add(at(sp, "run"), scriptExpr)
		}
		c.environment(sp, s.Env, s.Pass)
		c.steps(at(sp, "block"), s.Block, ids)
		c.steps(at(sp, "rescue"), s.Rescue, ids)
		c.steps(at(sp, "always"), s.Always, ids)
	}
}

func (c *checker) moves() {
	seen := map[string]bool{}
	for i, m := range c.p.Moved {
		mp := []string{"moved", strconv.Itoa(i)}
		for k, a := range map[string]string{"from": m.From, "to": m.To} {
			if !address.MatchString(a) {
				c.add(at(mp, k), "%s is not an address such as web, image[lxc], or dns/zone", a)
			}
		}
		if c.p.Units[m.From] != nil {
			c.add(at(mp, "from"), "%s is still a unit; moved: is for addresses that left the file", m.From)
		}
		if m.From == m.To {
			c.add(mp, "from and to are the same address")
		}
		chained := slices.ContainsFunc(c.p.Moved[i+1:], func(n *Move) bool { return n.From == m.To })
		if c.p.Units[unitOf(m.To)] == nil && !chained {
			c.add(at(mp, "to"), "there is no unit %s, and no later moved: entry starts at it", m.To)
		}
		if seen[m.From] {
			c.add(at(mp, "from"), "%s is moved twice", m.From)
		}
		seen[m.From] = true
	}
	for i, r := range c.p.Removed {
		if c.p.Units[r.From] != nil {
			c.add([]string{"removed", strconv.Itoa(i), "from"}, "%s is still a unit; removed: is for units that left the file", r.From)
		}
	}
}

// unitOf returns the unit name at the start of an address.
func unitOf(addr string) string {
	name, _, _ := strings.Cut(addr, "/")
	name, _, _ = strings.Cut(name, "[")
	return name
}

// unsupported rejects what later versions of OpsTruss run; the keys are in
// the schema already.
func (c *checker) unsupported() {
	p := c.p
	if len(p.Uses) > 0 {
		c.add([]string{"uses"}, "not supported yet")
	}
	if len(p.Secrets) > 0 {
		c.add([]string{"secrets"}, "not supported yet")
	}
	if p.Backend != nil && !p.Backend.Local {
		c.add([]string{"backend"}, "plugin backends are not supported yet")
	}
	if p.MCP != nil {
		c.add([]string{"mcp"}, "not supported yet")
	}
	c.sensitiveInputs([]string{"inputs"}, p.Inputs)
	for _, name := range slices.Sorted(maps.Keys(p.Outputs)) {
		if p.Outputs[name].Sensitive {
			c.add([]string{"outputs", name, "sensitive"}, "sensitive values are not supported yet")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Units)) {
		u, path := p.Units[name], []string{"units", name}
		if u.Uses != "" {
			c.add(at(path, "uses"), "units that use a plugin, task, or component are not supported yet")
		}
		if u.Service != nil {
			c.add(at(path, "service"), "service units are not supported yet")
		}
		c.sensitiveOutputs(at(path, "outputs"), u.Outputs)
		for _, tn := range slices.Sorted(maps.Keys(u.Tasks)) {
			c.unsupportedTask(at(path, "tasks", tn), u.Tasks[tn])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		c.unsupportedTask([]string{"tasks", name}, p.Tasks[name])
	}
}

func (c *checker) unsupportedTask(path []string, t *Task) {
	if t.On != nil {
		c.add(at(path, "on"), "triggers are not supported yet")
	}
	c.unsupportedCalls(path, t.Calls)
	c.sensitiveInputs(at(path, "inputs"), t.Inputs)
	c.sensitiveOutputs(at(path, "outputs"), t.Outputs)
	var walk func([]string, []*Step)
	walk = func(path []string, steps []*Step) {
		for i, s := range steps {
			sp := at(path, strconv.Itoa(i))
			c.unsupportedCalls(sp, s.Calls)
			c.sensitiveOutputs(at(sp, "outputs"), s.Outputs)
			walk(at(sp, "block"), s.Block)
			walk(at(sp, "rescue"), s.Rescue)
			walk(at(sp, "always"), s.Always)
		}
	}
	walk(at(path, "steps"), t.Steps)
}

func (c *checker) unsupportedCalls(path []string, calls Calls) {
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if _, hinted := ansible[name]; !hinted {
			c.add(at(path, name), "calling a task by name is not supported yet; write run, or task: for a task of this project")
		}
	}
}

func (c *checker) sensitiveInputs(path []string, inputs map[string]*Input) {
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		in, ip := inputs[name], at(path, name)
		if in.Sensitive {
			c.add(at(ip, "sensitive"), "sensitive values are not supported yet")
		}
		if in.Items != nil {
			c.sensitiveInputs(ip, map[string]*Input{"items": in.Items})
		}
		c.sensitiveInputs(at(ip, "properties"), in.Properties)
	}
}

func (c *checker) sensitiveOutputs(path []string, outputs map[string]*Output) {
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		if outputs[name].Sensitive {
			c.add(at(path, name, "sensitive"), "sensitive values are not supported yet")
		}
	}
}
