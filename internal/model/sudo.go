package model

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// AsksSudo reports whether a program of t runs as root where it may: t
// writes sudo: true, or a step in it does.
func (t *Task) AsksSudo() bool { return t.Sudo || stepsAskSudo(t.Steps) }

func stepsAskSudo(steps []*Step) bool {
	return slices.ContainsFunc(steps, func(s *Step) bool {
		return s.Sudo != nil && *s.Sudo || stepsAskSudo(s.Block) || stepsAskSudo(s.Rescue) || stepsAskSudo(s.Always)
	})
}

// allowsSudo reports whether the unit, or the declaration in uses: of what
// it uses, writes sudo: true.
func (u *Unit) allowsSudo() bool {
	return u.Sudo || u.Target != nil && u.Target.Package != nil && u.Target.Package.Decl != nil && u.Target.Package.Decl.Sudo
}

// SudoRuns reports whether a program that asks for sudo runs as root: in
// the files of the project it does, and in a component from git (remote)
// only when the units above it allow it (up).
func SudoRuns(asks, remote, up bool) bool { return asks && (!remote || up) }

// SudoAllows reports whether a place that may write sudo: true, as a unit
// or a step, lets what it calls run as root when that asks for it: in the
// files of the project, when it writes sudo: true (local) or the units
// above it allow it; in a component from git, only when both do.
func SudoAllows(local, remote, up bool) bool {
	if remote {
		return local && up
	}
	return local || up
}

// SudoAllowed reports whether what n uses runs as root where it asks for
// it: n or the declaration in uses: of what it uses writes sudo: true, or,
// in the files of the project, a unit that uses a component n is in does;
// in a component from git, both n and the unit that uses the component.
// A nil n, above the project, allows nothing.
func (n *Node) SudoAllowed() bool {
	if n == nil {
		return false
	}
	return SudoAllows(n.Unit.allowsSudo(), n.In.Source != nil, n.Parent.SudoAllowed())
}

// SudoRun reports whether the program of n, which its run or command
// gives, and that of its destroy, run as root; a unit that uses anything
// else has no program of its own.
func (n *Node) SudoRun() bool {
	t := n.Unit.Target
	return (t == nil || t.Kind == TargetBuiltin) && SudoRuns(n.Unit.Sudo, n.In.Source != nil, n.Parent.SudoAllowed())
}

// sudoUnit rejects sudo on a unit whose programs it cannot run as root: a
// service lives in the daemon, and a built-in task other than command runs
// inside OpsTruss.
func (c *checker) sudoUnit(path []string, u *Unit) {
	switch {
	case !u.Sudo:
	case u.Service != nil:
		c.add(at(path, "sudo"), "sudo is for the programs of a run; a service runs in the daemon")
	case u.Target != nil && u.Target.Kind == TargetBuiltin && u.Target.Task != "command":
		c.add(at(path, "sudo"), "%s runs inside OpsTruss, not as a program; sudo is for programs", u.Uses)
	}
}

// sudoBuiltin rejects sudo written on a call of a built-in task other than
// command, which runs inside OpsTruss.
func (c *checker) sudoBuiltin(path []string, calls Calls) {
	for name := range calls {
		if t, err := c.call(name); err == nil && t.Kind == TargetBuiltin && t.Task != "command" {
			c.add(path, "%s runs inside OpsTruss, not as a program; sudo is for programs", name)
		}
	}
}

// sudo checks, once the units are a graph, that what asks for sudo is
// allowed: a unit that uses a plugin, a task of one, or a component from
// git that asks, and a step or task that calls a task of a plugin that
// asks, write sudo: true or are inside a unit that allows it. In a
// component from git, the place that calls asks in turn, so the unit that
// uses the component allows it; a problem is reported once, where it is
// written.
func (c *checker) sudo() {
	seen := map[Problem]bool{}
	report := func(in *Project, path []string, format string, args ...any) {
		if pr := in.problem(path, format, args...); !seen[pr] {
			seen[pr] = true
			c.probs = append(c.probs, pr)
		}
	}
	c.sudoTasks(c.p, c.p.Tasks, "tasks", false, false, report)
	for _, path := range slices.Sorted(maps.Keys(c.p.Nodes)) {
		n := c.p.Nodes[path]
		remote, up := n.In.Source != nil, n.Parent.SudoAllowed()
		local := n.Unit.allowsSudo()
		// In a component from git, a unit that allows asks in turn, and the
		// unit above reports what is not allowed.
		if what := n.sudoAsks(); what != "" && !local && (remote || !up) {
			report(n.In, []string{"units", n.Name}, "unit %s: %s; allow it with sudo: true on the unit", n.Path, what)
		}
		c.sudoTasks(n.In, n.Unit.Tasks, "units."+n.Name+".tasks", remote, up, report)
		if comp := n.Component(); comp != nil {
			c.sudoTasks(comp, comp.Tasks, "tasks", comp.Source != nil, n.SudoAllowed(), report)
		}
	}
}

// sudoAsks says what the unit of n uses that asks for sudo, or "": a
// plugin or a task of one with tasks that write it, or a component from
// git whose files write it.
func (n *Node) sudoAsks() string {
	t := n.Unit.Target
	switch {
	case t == nil:
	case t.Kind == TargetPlugin:
		var asks []string
		for _, name := range slices.Sorted(maps.Keys(t.Package.Plugin.Tasks)) {
			if !slices.Contains(Hooks, name) && t.Package.Plugin.Tasks[name].AsksSudo() {
				asks = append(asks, name)
			}
		}
		if len(asks) > 0 {
			return fmt.Sprintf("plugin %s runs %s with sudo", t.Package.Name, andList(asks))
		}
	case t.Kind == TargetTask:
		if t.Package.Plugin.Tasks[t.Task].AsksSudo() {
			return fmt.Sprintf("task %s runs with sudo", t.Name)
		}
	case t.Kind == TargetComponent && t.Component != nil && t.Component.Source != nil:
		if asks := t.Component.sudoAsks(); len(asks) > 0 {
			return fmt.Sprintf("component %s asks for sudo in %s", t.Name, andList(asks))
		}
	}
	return ""
}

// sudoAsks lists what the files of p write sudo: true on: its units, their
// tasks, its tasks, and its declarations in uses:.
func (p *Project) sudoAsks() []string {
	var asks []string
	for _, name := range slices.Sorted(maps.Keys(p.Uses)) {
		if d := p.Uses[name]; d != nil && d.Sudo {
			asks = append(asks, "uses."+name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Units)) {
		u := p.Units[name]
		if u.Sudo || slices.ContainsFunc(slices.Collect(maps.Values(u.Tasks)), (*Task).AsksSudo) {
			asks = append(asks, "units."+name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		if p.Tasks[name].AsksSudo() {
			asks = append(asks, "tasks."+name)
		}
	}
	return asks
}

// sudoTasks checks the calls of tasks of plugins in tasks, which the files
// of in write at path: remote and up are as for SudoAllows.
func (c *checker) sudoTasks(in *Project, tasks map[string]*Task, path string, remote, up bool,
	report func(*Project, []string, string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(tasks)) {
		t, tp := tasks[name], append(strings.Split(path, "."), name)
		for callee := range t.Calls {
			c.sudoCall(in, tp, callee, t.Sudo, remote, up, "task", report)
		}
		c.sudoSteps(in, at(tp, "steps"), t.Steps, t.Sudo, remote, up, report)
	}
}

func (c *checker) sudoSteps(in *Project, path []string, steps []*Step, sudo, remote, up bool,
	report func(*Project, []string, string, ...any)) {
	for i, s := range steps {
		sp, own := at(path, strconv.Itoa(i)), sudo
		if s.Sudo != nil {
			own = *s.Sudo
		}
		for callee := range s.Calls {
			c.sudoCall(in, sp, callee, own, remote, up, "step", report)
		}
		c.sudoSteps(in, at(sp, "block"), s.Block, own, remote, up, report)
		c.sudoSteps(in, at(sp, "rescue"), s.Rescue, own, remote, up, report)
		c.sudoSteps(in, at(sp, "always"), s.Always, own, remote, up, report)
	}
}

// sudoCall checks one call of a task by name in the files of in, by a step
// or task (what) whose sudo is local.
func (c *checker) sudoCall(in *Project, path []string, callee string, local, remote, up bool, what string,
	report func(*Project, []string, string, ...any)) {
	t, err := in.Resolve(callee)
	if err != nil || t.Kind != TargetTask || !t.Package.Plugin.Tasks[t.Task].AsksSudo() {
		return
	}
	local = local || t.Package.Decl != nil && t.Package.Decl.Sudo
	if !local && (remote || !up) {
		report(in, path, "task %s runs with sudo; allow it with sudo: true on the %s", callee, what)
	}
}

// SudoUsers lists what runs programs as root in p: the units, by path,
// whose program runs as root or whose plugin or task runs as root where it
// asks, and the tasks of the project that write sudo: true.
func (p *Project) SudoUsers() []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(p.Nodes)) {
		n := p.Nodes[path]
		if t := n.Unit.Target; n.SudoRun() || t != nil && t.Kind != TargetComponent && n.sudoAsks() != "" && n.SudoAllowed() {
			out = append(out, "units."+path)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Tasks)) {
		if p.Tasks[name].AsksSudo() {
			out = append(out, "tasks."+name)
		}
	}
	return out
}
