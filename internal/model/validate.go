package model

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/robfig/cron/v3"
)

// BuiltinTasks are the tasks in the binary, also called opstruss.<name>.
var BuiltinTasks = []string{"command", "http", "read", "write", "set", "assert", "wait", "log", "kv"}

// StandardTasks are the lifecycle tasks of units.
var StandardTasks = []string{"plan", "up", "down", "status", "logs"}

// BuiltinFlags are the flags of OpsTruss on the command of a task, which no
// input of the task may make.
var BuiltinFlags = []string{"stack", "input", "json", "yes", "locked", "no-deps", "completion", "help", "event"}

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
	address = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\[[a-z0-9._-]+\])?(/[a-z][a-z0-9_-]*(\[[a-z0-9._-]+\])?)*$`)
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

// checker collects the problems of a project, or of a plugin file when
// plugin is set; then paths are in doc, and its tasks read only their
// inputs and steps.
type checker struct {
	p      *Project
	plugin *Plugin
	doc    *document
	pkgs   map[string]*Package // read packages, by directory
	comps  *components
	probs  Problems

	readsSecret  map[string]any // the locals that read a secret, once asked
	readsProgram map[string]any // the locals that read a secret from a program or a task, once asked
}

// components are the components read for a project, by directory, shared
// by the checkers of its files; a component whose files have problems is
// nil, and reading holds those being read, so a component that uses
// itself is found.
type components struct {
	read    map[string]*Project
	reading map[string]bool
}

func (c *checker) add(path []string, format string, args ...any) {
	if c.doc != nil {
		c.probs = append(c.probs, c.doc.problem(path, format, args...))
		return
	}
	c.probs = append(c.probs, c.p.problem(path, format, args...))
}

func at(path []string, more ...string) []string { return append(slices.Clone(path), more...) }

func (p *Project) check() Problems {
	c := &checker{p: p, pkgs: map[string]*Package{}, comps: &components{read: map[string]*Project{}, reading: map[string]bool{}}}
	c.file()
	c.graph()
	slices.SortStableFunc(c.probs, func(a, b Problem) int {
		return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Column, b.Column))
	})
	return c.probs
}

// file checks the files of the project or of one component.
func (c *checker) file() {
	c.p.Packages = map[string]*Package{}
	c.root()
	if c.p.Source != nil {
		c.pluginInputs([]string{"inputs"}, c.p.Inputs)

	}
	c.uses()
	c.stacks()
	c.units()
	c.tasks()
	c.secrets()
	c.encryptionKeys()
	c.moves()
	c.references()
	c.unsupported()
	c.sensitiveValues()
}

// component reads the component in dir once and checks its files as a
// file of their own: the names in its uses: are its own, and what it
// reads comes in through its inputs.
func (c *checker) component(dir string, source *sources.Identity, sourceRoot string) (*Project, error) {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	cs := c.comps
	key := dir
	if source != nil {
		key += "@" + source.Ref
	}
	if cs.reading[dir] {
		return nil, fmt.Errorf("%s uses itself, through the components it uses", c.rel(dir))
	}
	if p, ok := cs.read[key]; ok {
		if p == nil {
			return nil, errBroken
		}
		return p, nil
	}
	cs.reading[dir] = true
	defer delete(cs.reading, dir)

	p, probs, err := readComponent(c.p.base, dir, c.p.StackName, c.p.Sources, source, sourceRoot)
	var pp Problems
	if errors.As(err, &pp) {
		probs, err = pp, nil
	}
	if err != nil {
		return nil, err
	}
	if len(probs) > 0 {
		c.probs = append(c.probs, probs...)
		cs.read[key] = nil
		return nil, errBroken
	}
	cc := &checker{p: p, pkgs: c.pkgs, comps: cs}
	cc.file()
	c.probs = append(c.probs, cc.probs...)
	cs.read[key] = p
	return p, nil
}

func (c *checker) root() {
	if c.p.Name == "" && !c.p.component {
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
		decl := c.p.Uses[name]
		c.environment(path, decl.Env, decl.Pass)
		src := decl.Source
		var pkg *Package
		switch {
		case localPath(src):
			var err error
			if pkg, err = c.pkg(name, decl, filepath.Join(c.p.DirOf("uses."+name), src)); err != nil {
				c.add(path, "%v", err)
				continue
			}
			c.packageNames(path, pkg)
		case strings.HasPrefix(src, "opstruss."):
			if Builtin(strings.TrimPrefix(src, "opstruss.")) == nil {
				c.add(path, "there is no built-in task %s", strings.TrimPrefix(src, "opstruss."))
				continue
			}
			pkg = &Package{Name: name, Decl: decl, Builtin: strings.TrimPrefix(src, "opstruss.")}
		case strings.HasPrefix(src, "github.com/"):
			var err error
			if pkg, err = c.sourcePackage(name, decl, src); err != nil {
				c.add(path, "%v", err)
				continue
			}
			c.packageNames(path, pkg)
		default:
			c.add(path, "a source is ./<path>, ../<path>, github.com/opstruss/<repo>@<ref>, or opstruss.<task>")
			continue
		}
		c.declaredWith(path, pkg)
		c.p.Packages[name] = pkg
	}
}

func (c *checker) stacks() {
	if c.p.component {
		return
	}
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
	if c.plugin != nil && len(pass) > 0 {
		c.add(at(path, "pass"), "pass: is not in %s: a plugin asks for variables with env:, and the project lets them through with pass:", PluginFile)
	}
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

// unitCommands are the programs of a unit, by their keys.
func unitCommands(u *Unit) map[string]*Command {
	cmds := map[string]*Command{"run": u.Run, "service": u.Service, "down": u.Down}
	if u.Ready != nil {
		cmds["ready.run"] = u.Ready.Run
	}
	if u.Stop != nil {
		cmds["stop.run"] = u.Stop.Run
	}
	return cmds
}

// ready checks that ready: has exactly one check, and that the address
// of an http or tcp check written without an expression is one.
func (c *checker) ready(path []string, r *Ready) {
	if r == nil {
		return
	}
	n := 0
	for _, set := range []bool{r.HTTP != "", r.TCP != "", r.Run != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		c.add(path, "ready has exactly one of http, tcp, or run")
	}
	if r.HTTP != "" && !expr.HasExpr(r.HTTP) {
		if u, err := url.Parse(r.HTTP); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			c.add(at(path, "http"), "%s is not an http or https URL", r.HTTP)
		}
	}
	if r.TCP != "" && !expr.HasExpr(r.TCP) {
		if _, _, err := net.SplitHostPort(r.TCP); err != nil {
			c.add(at(path, "tcp"), "%s is not host:port: %v", r.TCP, err)
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
		if c.p.Source != nil && u.Approval == "auto" {
			c.add(at(path, "approval"), "remote components cannot grant approval")
		}
		if name == "tool" {
			c.add(path, "tool is the directory where a plugin with state: tool keeps the tool's state; give the unit another name")
		}
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
			if u.Timeout != "" {
				c.add(at(path, "timeout"), "a service runs until it stops; ready.timeout bounds how long up waits for it")
			}
			if u.Retry != nil {
				c.add(at(path, "retry"), "a service starts again by restart:, not retry:")
			}
			c.ready(at(path, "ready"), u.Ready)
		}
		if u.Uses != "" {
			c.unitUses(path, name, u)
		} else if u.With != nil {
			c.add(at(path, "with"), "with is for units that use a plugin, task, or component")
		}
		if len(u.Outputs) > 0 && u.Run == nil {
			c.add(at(path, "outputs"), "outputs: is for run units; other units get their outputs from what they run")
		}
		for i, n := range u.Needs {
			if c.p.Units[n] == nil {
				c.add(at(path, "needs", strconv.Itoa(i)), "there is no unit %s", n)
			}
		}
		for k, cmd := range unitCommands(u) {
			if hasExpr(cmd) {
				c.add(at(path, strings.Split(k, ".")...), scriptExpr)
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

// unitUses finds what a unit uses and checks the inputs it gives.
func (c *checker) unitUses(path []string, name string, u *Unit) {
	var t *Target
	var err error
	if strings.HasPrefix(u.Uses, "github.com/") {
		var pkg *Package
		if pkg, err = c.sourcePackage(u.Uses, nil, u.Uses); err == nil {
			t, err = unitTarget(u.Uses, pkg)
		}
	} else if localPath(u.Uses) {
		var pkg *Package
		if pkg, err = c.pkg(u.Uses, nil, filepath.Join(c.p.DirOf("units."+name), u.Uses)); err == nil {
			c.packageNames(at(path, "uses"), pkg)
			t, err = unitTarget(u.Uses, pkg)
		}
	} else {
		t, err = c.p.resolve(u.Uses, true)
	}
	if err != nil {
		if !errors.Is(err, errBroken) {
			c.add(at(path, "uses"), "%v", err)
		}
		return
	}
	if t.Kind == TargetComponent {
		comp, err := c.component(t.Dir, t.Package.Source, t.Package.SourceRoot)
		if err != nil {
			if !errors.Is(err, errBroken) {
				c.add(at(path, "uses"), "%v", err)
			}
			return
		}
		t.Component = comp
		c.componentKeys(path, u)
	}
	u.Target = t
	c.with(at(path, "with"), at(path, "uses"), t, u.With)
	if t.Kind == TargetProject {
		if cycle := c.unitTaskCycle(t.Task, map[string]bool{}); cycle != "" {
			c.add(at(path, "uses"), "%s, so a unit that uses it would run units at its own turn", cycle)
		}
	}
}

// componentKeys rejects what a unit that uses a component does not take:
// the units inside have their own programs, and the unit gives them only
// what the table of keys of a component unit says.
func (c *checker) componentKeys(path []string, u *Unit) {
	for _, k := range []struct {
		key string
		set bool
	}{{"requires", u.Requires != nil}, {"shell", u.Shell != ""}, {"dir", u.Dir != ""}, {"stdin", u.Stdin != nil},
		{"sources", len(u.Sources) > 0}, {"generates", len(u.Generates) > 0}, {"revision", u.Revision != ""}, {"down", u.Down != nil}} {
		if k.set {
			c.add(at(path, k.key), "%s is not for a unit that uses a component; the units inside have their own", k.key)
		}
	}
	if len(u.Tasks) > 0 {
		c.add(at(path, "tasks"), "a unit that uses a component has the component's tasks; write them in the component")
	}
	if u.Retry != nil && u.Retry.Until != nil {
		c.add(at(path, "retry", "until"), "retry.until judges the outputs of one program; write it in the units inside")
	}
}

func (c *checker) tasks() {
	webhooks := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.p.Tasks)) {
		t, path := c.p.Tasks[name], []string{"tasks", name}
		if slices.Contains(Commands, name) || slices.Contains(StandardTasks, name) {
			c.add(path, "%s is a built-in command or a standard task; give the task another name", name)
		}
		c.task(path, t, !c.p.component)
		if t.On == nil {
			continue
		}
		c.schedules(at(path, "on"), t.On)
		c.watches(at(path, "on"), t.On)
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
	if len(webhooks) > 0 && (c.p.Runtime == nil || c.p.Runtime.Listen == "") {
		c.add([]string{"runtime", "listen"}, "a task has on: webhook, so the daemon needs runtime.listen, the address it listens on, such as 127.0.0.1:7878")
	}
	if c.p.Runtime != nil && c.p.Runtime.Listen != "" {
		if _, _, err := net.SplitHostPort(c.p.Runtime.Listen); err != nil {
			c.add([]string{"runtime", "listen"}, "%s is not host:port: %v", c.p.Runtime.Listen, err)
		}
	}
}

// schedules checks the schedule and every triggers of a task: cron as
// CronJob of Kubernetes reads it, with its time zone in timezone: only and
// intervals in every: only, and intervals of at least a second.
func (c *checker) schedules(path []string, on *Triggers) {
	for i, s := range on.Schedule {
		sp := at(path, "schedule")
		if len(on.Schedule) > 1 {
			sp = at(sp, strconv.Itoa(i))
		}
		switch spec := s.Cron; {
		case strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ="):
			c.add(at(sp, "cron"), "write the time zone in timezone:, not in cron")
		case strings.HasPrefix(spec, "@every"):
			c.add(at(sp, "cron"), "write an interval in every:, not in cron")
		case expr.HasExpr(spec):
			c.add(at(sp, "cron"), "${{ }} is not allowed in cron; a daemon reads the schedule as it is written")
		default:
			if _, err := cron.ParseStandard(spec); err != nil {
				c.add(at(sp, "cron"), "%v", err)
			}
		}
		if tz := s.Timezone; tz != "" {
			if expr.HasExpr(tz) {
				c.add(at(sp, "timezone"), "${{ }} is not allowed in timezone")
			} else if _, err := time.LoadLocation(tz); err != nil {
				c.add(at(sp, "timezone"), "%s is not a time zone of the IANA database, such as Asia/Jakarta", tz)
			}
		}
	}
	for i, every := range on.Every {
		ep := at(path, "every")
		if len(on.Every) > 1 {
			ep = at(ep, strconv.Itoa(i))
		}
		if d, err := time.ParseDuration(every); err == nil && d < time.Second {
			c.add(ep, "every is at least 1s")
		}
	}
}

// watches checks the watch and after triggers of a task: a watch has
// exactly one source, stream goes with unit, batch or debounce but not
// both, and the units and tasks they name exist.
func (c *checker) watches(path []string, on *Triggers) {
	for i, w := range on.Watch {
		wp := at(path, "watch")
		if len(on.Watch) > 1 {
			wp = at(wp, strconv.Itoa(i))
		}
		n := len(w.Calls)
		for _, set := range []bool{w.Unit != "", len(w.Paths) > 0} {
			if set {
				n++
			}
		}
		if n != 1 {
			c.add(wp, "a watch has exactly one source: unit, paths, or one call of a task")
		}
		if w.Unit != "" && c.p.Units[w.Unit] == nil {
			c.add(at(wp, "unit"), "there is no unit %s", w.Unit)
		}
		if w.Stream != "" && w.Unit == "" {
			c.add(at(wp, "stream"), "stream is for a watch of a unit")
		}
		if w.Batch != "" && w.Debounce != "" {
			c.add(wp, "a watch has batch or debounce, not both")
		}
		for name := range w.Calls {
			if slices.Contains(StandardTasks, name) || slices.Contains(BuiltinTasks, name) && name != "command" {
				c.add(at(wp, name), "a watch source is a program that writes a line per event: command, or a task of a plugin")
			}
		}
	}
	for i, a := range on.After {
		ap := at(path, "after")
		if len(on.After) > 1 {
			ap = at(ap, strconv.Itoa(i))
		}
		if a.Unit != "" && c.p.Units[a.Unit] == nil {
			c.add(at(ap, "unit"), "there is no unit %s", a.Unit)
		}
		if c.p.Tasks[a.Task] == nil && !slices.Contains(StandardTasks, a.Task) && !c.unitTask(a.Task) {
			c.add(at(ap, "task"), "there is no task %s", a.Task)
		}
	}
}

func callNames(calls Calls) string {
	return strings.Join(slices.Sorted(maps.Keys(calls)), " and ")
}

func (c *checker) task(path []string, t *Task, project bool) {
	if c.p != nil && c.p.Source != nil {
		c.pluginInputs(at(path, "inputs"), t.Inputs)
		if t.Approval == "auto" {
			c.add(at(path, "approval"), "remote components cannot grant approval")
		}
	}
	kinds := len(t.Calls)
	if t.Run != nil {
		kinds++
	}
	if len(t.Steps) > 0 {
		kinds++
	}
	c.hints(path, t.Calls)
	c.calls(path, t.Calls)
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
	c.flags(path, t.Inputs)
	vars := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(t.Inputs)) {
		if c.plugin == nil && c.p.Inputs[name] != nil {
			c.add(at(path, "inputs", name), "%s is an input of the %s already; a task adds inputs of other names", name,
				map[bool]string{true: "component", false: "project"}[c.p.component])
		}
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
		if c.plugin != nil {
			break // pluginRules rejects needs
		}
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

// flags checks the flags that the inputs of a task make on its command,
// as argparse refuses a conflicting option string: none is a flag of
// OpsTruss, and no two inputs make the same one.
func (c *checker) flags(path []string, inputs map[string]*Input) {
	from := map[string]string{}
	for _, f := range BuiltinFlags {
		from[f] = "a flag of OpsTruss"
	}
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		flags := []string{FlagName(name)}
		if inputs[name].Type == "boolean" {
			flags = append(flags, "no-"+flags[0])
		}
		for _, f := range flags {
			if other, dup := from[f]; dup {
				c.add(at(path, "inputs", name), "input %s makes the flag --%s, which is %s; rename the input", name, f, other)
				continue
			}
			from[f] = "the flag of input " + name
		}
	}
}

func (c *checker) hints(path []string, calls Calls) {
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if hint, ok := ansibleHint(name); ok {
			c.add(at(path, name), "%s", hint)
		}
	}
}

// calls finds what each call of a task by name runs and checks its
// inputs.
func (c *checker) calls(path []string, calls Calls) {
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if _, hinted := ansible[name]; hinted {
			continue
		}
		t, err := c.call(name)
		if err != nil {
			if !errors.Is(err, errBroken) {
				c.add(at(path, name), "%v", err)
			}
			continue
		}
		c.with(at(path, name), at(path, name), t, calls[name])
	}
}

// call finds what a call runs; a plugin, which has no uses:, calls only
// built-in tasks.
func (c *checker) call(name string) (*Target, error) {
	if c.plugin == nil {
		return c.p.Resolve(name)
	}
	short := strings.TrimPrefix(name, "opstruss.")
	if Builtin(short) == nil {
		return nil, fmt.Errorf("a plugin task calls only built-in tasks; there is no built-in task %s", name)
	}
	return &Target{Name: name, Kind: TargetBuiltin, Task: short}, nil
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

// secretKeys are the keys of a secret that are not a call of a task.
var secretKeys = []string{"sops", "field", "env", "run", "pass", "dir", "shell"}

// schemaHint says what the schema means where its words do not: a key of
// a secret that reads as a call of a task, of a plugin that is not there,
// is most often a typo of a source; and encryption_keys says what it
// holds.
func schemaHint(path []string, msg string, doc map[string]any) (string, bool) {
	n := len(path)
	var keys []string // the path within encryption_keys
	isKeys := true
	switch {
	case n >= 1 && path[0] == "encryption_keys":
		keys = path[1:]
	case n >= 3 && path[0] == "stacks" && path[2] == "encryption_keys":
		keys = path[3:]
	default:
		isKeys = false
	}
	switch {
	case (n == 3 && path[0] == "secrets" || n == 5 && path[0] == "stacks" && path[2] == "secrets") &&
		!slices.Contains(secretKeys, path[n-1]) && !strings.HasPrefix(path[n-1], "x-"):
		if _, ok := valueAt(doc, path).(map[string]any); ok {
			return "", false
		}
		hint := path[n-1] + " is not a source of a secret: sops, env, run, or a call of a task of a plugin, which takes a map"
		if near := nearest(path[n-1], secretKeys); near != "" {
			hint += "; did you mean " + near + "?"
		}
		return hint, true
	case !isKeys:
	case len(keys) == 0 && strings.HasPrefix(msg, "minItems"):
		return "encryption_keys lists at least one key; without it, the data key is wrapped for the age key of the machine that makes it", true
	case len(keys) == 1 && (strings.HasPrefix(msg, "maxProperties") || strings.HasPrefix(msg, "minProperties")):
		return "an entry of encryption_keys that sops wraps for has one key: kms, gcp_kms, azure_keyvault, hc_vault_transit_uri, or pgp", true
	case len(keys) == 2 && msg == "unknown key":
		return keys[1] + " is not a key that sops wraps for: kms, gcp_kms, azure_keyvault, hc_vault_transit_uri, or pgp", true
	}
	return "", false
}

// nearest returns the word of words that s is a typo of, at most two edits
// away, or "".
func nearest(s string, words []string) string {
	best, bestD := "", 3
	for _, w := range words {
		if d := distance(s, w); d < bestD && d < len(w) {
			best, bestD = w, d
		}
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func (c *checker) steps(path []string, steps []*Step, ids map[string]bool) {
	for i, s := range steps {
		sp := at(path, strconv.Itoa(i))
		if c.p != nil && c.p.Source != nil && s.Yes {
			c.add(at(sp, "yes"), "remote components cannot grant approval")
		}
		kinds := 0
		for _, set := range []bool{s.Run != nil, s.Task != "", len(s.Block) > 0, len(s.Calls) > 0} {
			if set {
				kinds++
			}
		}
		c.hints(sp, s.Calls)
		c.calls(sp, s.Calls)
		if c.plugin != nil && s.Task != "" {
			c.add(at(sp, "task"), "a plugin task calls no task: chains belong to the project, and a plugin that needs one brings a component")
		}
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

// secrets checks each source of a secret: exactly one of sops, env, run,
// or a call of a task of a plugin that gives the output value, each with
// the keys that belong to it.
func (c *checker) secrets() {
	for _, name := range slices.Sorted(maps.Keys(c.p.Secrets)) {
		s, path := c.p.Secrets[name], []string{"secrets", name}
		kinds := len(s.Calls)
		for _, set := range []bool{s.Sops != "", s.EnvName != "", s.Run != nil} {
			if set {
				kinds++
			}
		}
		c.hints(path, s.Calls)
		c.calls(path, s.Calls)
		switch {
		case len(s.Calls) > 1:
			c.add(path, "a secret calls one task; found %s", callNames(s.Calls))
		case kinds != 1:
			c.add(path, "a secret has exactly one source: sops, env, run, or one call of a task of a plugin")
		}
		if s.Field != "" && s.Sops == "" {
			c.add(at(path, "field"), "field: reads one field of a sops file")
		}
		if s.Run == nil && len(s.Calls) == 0 {
			if len(s.Env) > 0 {
				c.add(at(path, "env"), "env: as variables is for the program of run or of a call; env: <NAME> reads a variable")
			}
			if len(s.Pass) > 0 {
				c.add(at(path, "pass"), "pass: is for the program of run or of a call")
			}
		}
		if s.Run == nil && (s.Dir != "" || s.Shell != "") {
			c.add(path, "dir: and shell: are for run")
		}
		if hasExpr(s.Run) {
			c.add(at(path, "run"), scriptExpr)
		}
		c.environment(path, s.Env, s.Pass)
		for _, callee := range slices.Sorted(maps.Keys(s.Calls)) {
			t, err := c.call(callee)
			if err != nil {
				continue // calls reported it
			}
			if t.Kind != TargetTask || t.Package.Plugin.Tasks[t.Task].Outputs["value"] == nil {
				c.add(at(path, callee), "a secret comes from a task of a plugin with the output value, and %s is not one", callee)
			}
		}
	}
}

// encryptionKeys checks encryption_keys without any key: an age
// recipient that age reads, never printed, since a secret key can be
// pasted where its public key belongs, no expression, and no key twice.
// The schema checks the keys for sops.
func (c *checker) encryptionKeys() {
	seen := map[string]bool{}
	for i, k := range c.p.EncryptionKeys {
		path := []string{"encryption_keys", strconv.Itoa(i)}
		id := k.Age
		for kind, v := range k.Sops {
			id = kind + " " + v
		}
		switch {
		case expr.HasExpr(id):
			c.add(path, "encryption_keys are read before any expression, so they hold no ${{ }}")
			continue
		case k.Age != "":
			if err := secret.CheckRecipient(k.Age); err != nil {
				c.add(path, "%v", err)
				continue
			}
		}
		if seen[id] {
			c.add(path, "this key is in encryption_keys already")
		}
		seen[id] = true
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
		if !strings.Contains(m.From, "[") && c.p.unitAt(m.From) {
			c.add(at(mp, "from"), "%s is still a unit; moved: is for addresses that left the file", m.From)
		}
		if m.From == m.To {
			c.add(mp, "from and to are the same address")
		}
		chained := slices.ContainsFunc(c.p.Moved[i+1:], func(n *Move) bool { return n.From == m.To })
		if !c.p.unitAt(m.To) && !chained {
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

// unsupported rejects what later versions of OpsTruss run; the keys are in
// the schema already.
func (c *checker) unsupported() {
	p := c.p
	if p.Backend != nil && !p.Backend.Local {
		c.add([]string{"backend"}, "plugin backends are not supported yet")
	}
	if p.MCP != nil {
		c.add([]string{"mcp"}, "not supported yet")
	}
}

// sensitiveValues keeps sensitive values out of the argv of programs,
// which other users can read in the list of processes.
func (c *checker) sensitiveValues() {
	p := c.p
	marks := map[string]any{}
	if in := inputMarks(p.Inputs); len(in) > 0 {
		marks["inputs"] = in
	}
	if len(p.Secrets) > 0 {
		marks["secrets"] = true
	}
	if l := p.LocalMarks(marks, nil); len(l) > 0 {
		marks["locals"] = l
	}
	for _, name := range slices.Sorted(maps.Keys(p.Units)) {
		u, path := p.Units[name], []string{"units", name}
		for key, cmd := range unitCommands(u) {
			if reads(argv(cmd), marks) {
				c.add(at(path, strings.Split(key, ".")...), "%s", argvMessage)
			}
		}
		for _, tn := range slices.Sorted(maps.Keys(u.Tasks)) {
			c.argvSensitive(at(path, "tasks", tn), u.Tasks[tn], marks)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		c.argvSensitive([]string{"tasks", name}, p.Tasks[name], marks)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Secrets)) {
		if reads(argv(p.Secrets[name].Run), marks) {
			c.add([]string{"secrets", name, "run"}, "%s", argvMessage)
		}
	}
}

// argvMessage says why a sensitive value is not an item of argv.
const argvMessage = "a sensitive value cannot be an item of argv, which other users can read in the list of processes; pass it through env or stdin"

// inputMarks marks the sensitive inputs among inputs.
func inputMarks(inputs map[string]*Input) map[string]any {
	out := map[string]any{}
	for name, in := range inputs {
		if in.Sensitive {
			out[name] = true
		}
	}
	return out
}

// reads says whether an expression in v reads a part that marks marks.
func reads(v any, marks map[string]any) bool {
	if len(marks) == 0 {
		return false
	}
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		return false
	}
	for _, r := range use.Refs {
		if expr.MarkAt(marks, r.Path...) != nil {
			return true
		}
	}
	return false
}

// argvSensitive rejects a sensitive value in an item of the argv of a
// program of t, whose own inputs add to what marks marks.
func (c *checker) argvSensitive(path []string, t *Task, marks map[string]any) {
	if in := inputMarks(t.Inputs); len(in) > 0 {
		all := map[string]any{}
		if m, ok := marks["inputs"].(map[string]any); ok {
			maps.Copy(all, m)
		}
		maps.Copy(all, in)
		marks = maps.Clone(marks)
		marks["inputs"] = all
	}
	if len(marks) == 0 {
		return
	}
	msg := argvMessage
	if reads(argv(t.Run), marks) {
		c.add(at(path, "run"), "%s", msg)
	}
	c.callArgv(path, t.Calls, marks, msg)
	var walk func([]string, []*Step)
	walk = func(sp []string, steps []*Step) {
		for i, s := range steps {
			p := at(sp, strconv.Itoa(i))
			if reads(argv(s.Run), marks) {
				c.add(at(p, "run"), "%s", msg)
			}
			c.callArgv(p, s.Calls, marks, msg)
			walk(at(p, "block"), s.Block)
			walk(at(p, "rescue"), s.Rescue)
			walk(at(p, "always"), s.Always)
		}
	}
	walk(at(path, "steps"), t.Steps)
}

func (c *checker) callArgv(path []string, calls Calls, marks map[string]any, msg string) {
	for _, name := range slices.Sorted(maps.Keys(calls)) {
		if t, err := c.call(name); err == nil && t.Kind == TargetBuiltin && t.Task == "command" && reads(calls[name]["argv"], marks) {
			c.add(at(path, name, "argv"), "%s", msg)
		}
	}
}
