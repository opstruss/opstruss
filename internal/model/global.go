package model

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/schema"
)

// GlobalScope is the scope of the global units and tasks of a project: one
// for every stack, with its records in .opstruss/state/_global/. Names of
// stacks start with a letter, so no stack takes it.
const GlobalScope = "_global"

// nameSections are the sections of names that a stack may add to or, for
// secrets, override.
var nameSections = []string{"inputs", "locals", "secrets", "units", "tasks", "outputs"}

// sections copies the sections of names of doc as the files of the
// project write them, before what a stack adds to the same maps.
func sections(doc map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, s := range nameSections {
		m, _ := doc[s].(map[string]any)
		out[s] = maps.Clone(m)
	}
	return out
}

// stackInputs are the stacks of root that give each input through with:.
func stackInputs(root map[string]any) map[string][]string {
	out := map[string][]string{}
	stacks, _ := root["stacks"].(map[string]any)
	for _, s := range slices.Sorted(maps.Keys(stacks)) {
		with, _ := mapAt(stacks, s, "with")
		for name := range with {
			out[name] = append(out[name], s)
		}
	}
	return out
}

// isGlobal says whether the unit or task written as v says global: true.
func isGlobal(v any) bool {
	m, _ := v.(map[string]any)
	g, _ := m["global"].(bool)
	return g
}

// HasGlobal says whether the project declares a global unit or task.
func (p *Project) HasGlobal() bool {
	for _, s := range []string{"units", "tasks"} {
		for _, v := range p.top[s] {
			if isGlobal(v) {
				return true
			}
		}
	}
	return false
}

// perStack is what the files of a project hold that differs by stack, and
// so what a global unit or task cannot read, with why for each name: the
// inputs that a stack gives through with:, the locals and secrets that
// read stack or anything else per stack, and the units and tasks that are
// not global.
type perStack struct {
	inputs  map[string]string
	locals  map[string]string
	secrets map[string]string
	units   map[string]bool
	tasks   map[string]bool
}

// stackParts finds what is per stack once.
func (p *Project) stackParts() *perStack {
	if p.perStack != nil {
		return p.perStack
	}
	ps := &perStack{inputs: map[string]string{}, locals: map[string]string{}, secrets: map[string]string{},
		units: map[string]bool{}, tasks: map[string]bool{}}
	for name, stacks := range p.stackWith {
		word := "stack"
		if len(stacks) > 1 {
			word = "stacks"
		}
		ps.inputs[name] = fmt.Sprintf("which %s %s give%s through with:", word, strings.Join(stacks, ", "), map[bool]string{true: "s"}[len(stacks) == 1])
	}
	for name, v := range p.top["units"] {
		ps.units[name] = !isGlobal(v)
	}
	for name, v := range p.top["tasks"] {
		ps.tasks[name] = !isGlobal(v)
	}
	// A local or a secret is per stack when it reads what is, through
	// others too, so this runs until nothing more is found.
	for found := true; found; {
		found = false
		for _, part := range []struct {
			section string
			why     map[string]string
		}{{"locals", ps.locals}, {"secrets", ps.secrets}} {
			for _, name := range slices.Sorted(maps.Keys(p.top[part.section])) {
				if part.why[name] != "" {
					continue
				}
				if w := ps.reads(p.top[part.section][name]); w != "" {
					part.why[name] = "which " + w
					found = true
				}
			}
		}
	}
	p.perStack = ps
	return ps
}

// reads says what of v is per stack, as "reads stack", or "".
func (ps *perStack) reads(v any) string {
	use, err := expr.AnalyzeValue(v)
	if err != nil {
		return ""
	}
	for _, r := range use.Refs {
		if w := ps.read(r); w != "" {
			return w
		}
	}
	return ""
}

// read says what is per stack in one reference, or "".
func (ps *perStack) read(r expr.Ref) string {
	name := ""
	if len(r.Path) > 1 {
		name = r.Path[1]
	}
	whole := func(m map[string]string, what string) string {
		if len(m) > 0 {
			return "reads " + what + " as a whole, which holds what differs by stack; read them by name"
		}
		return ""
	}
	switch r.Path[0] {
	case "stack":
		return "reads stack"
	case "inputs":
		if name == "" {
			return whole(ps.inputs, "inputs")
		}
		if ps.inputs[name] != "" {
			return "reads inputs." + name + ", " + ps.inputs[name]
		}
	case "locals":
		if name == "" {
			return whole(ps.locals, "locals")
		}
		if ps.locals[name] != "" {
			return "reads local " + name + ", " + ps.locals[name]
		}
	case "secrets":
		if ps.secrets[name] != "" {
			return "reads secret " + name + ", " + ps.secrets[name]
		}
	case "units":
		if ps.units[name] {
			return "reads units." + name + ", which is per stack"
		}
	case "tasks":
		if ps.tasks[name] {
			return "reads tasks." + name + ", which is per stack"
		}
	}
	return ""
}

// globalRead says what is wrong with r in a global unit or task, or "":
// it reads what is per stack, or what only this stack declares.
func (c *checker) globalRead(r expr.Ref) string {
	p := c.p
	if w := p.stackParts().read(r); w != "" {
		return w
	}
	if len(r.Path) < 2 || p.Global {
		return ""
	}
	// In a stack, what only the stack adds is there too.
	section := map[string]string{"inputs": "inputs", "locals": "locals", "secrets": "secrets", "units": "units", "tasks": "tasks"}[r.Path[0]]
	if section == "" {
		return ""
	}
	name := r.Path[1]
	if _, top := p.top[section][name]; top {
		return ""
	}
	if !slices.Contains(c.namesIn(section), name) {
		return "" // the check of what exists says so
	}
	return fmt.Sprintf("reads %s.%s, which only stack %s declares", r.Path[0], name, p.StackName)
}

// namesIn are the names of a section of the project as read for its stack.
func (c *checker) namesIn(section string) []string {
	p := c.p
	switch section {
	case "inputs":
		return slices.Collect(maps.Keys(p.Inputs))
	case "locals":
		return slices.Collect(maps.Keys(p.Locals))
	case "secrets":
		return slices.Collect(maps.Keys(p.Secrets))
	case "units":
		return slices.Collect(maps.Keys(p.Units))
	case "tasks":
		return slices.Collect(maps.Keys(p.Tasks))
	}
	return nil
}

// keepGlobal leaves in a project read for GlobalScope what that scope has:
// the global units and tasks, the inputs that no stack gives, and the
// locals, secrets, and outputs that read nothing per stack.
func (p *Project) keepGlobal() {
	ps := p.stackParts()
	keep := func(section string, ok func(name string, v any) bool) {
		m, _ := p.doc[section].(map[string]any)
		if m == nil {
			return
		}
		out := map[string]any{}
		for name, v := range m {
			if ok(name, v) {
				out[name] = v
			}
		}
		p.doc[section] = out
	}
	keep("units", func(_ string, v any) bool { return isGlobal(v) })
	keep("tasks", func(_ string, v any) bool { return isGlobal(v) })
	keep("inputs", func(name string, _ any) bool { return ps.inputs[name] == "" })
	keep("locals", func(name string, _ any) bool { return ps.locals[name] == "" })
	keep("secrets", func(name string, _ any) bool { return ps.secrets[name] == "" })
	keep("outputs", func(_ string, v any) bool {
		if m, ok := v.(map[string]any); ok {
			if value, has := m["value"]; has {
				v = value
			}
		}
		return ps.reads(v) == ""
	})
}

// level is the level of the file that the checker reads: an include file
// has that of its root.
func (c *checker) level() string {
	switch {
	case c.plugin != nil:
		return levelOr(c.plugin.Level)
	case c.p != nil:
		return levelOr(c.p.Level)
	}
	return schema.Oldest
}

func levelOr(l string) string {
	if l == "" {
		return schema.Oldest
	}
	return l
}

// since adds the problem of what a file of an older level than since does
// not have yet, as the schema says of a key.
func (c *checker) since(path []string, what, since string) bool {
	if !schema.Newer(since, c.level()) {
		return false
	}
	c.add(path, "%s needs opstruss: %q or later in this file, which says %s", what, since, c.level())
	return true
}

// globalUnit checks a unit with global: true: only in the files of the
// project, outside what one stack includes, and never a service, nor
// using a component that reads stack.
func (c *checker) globalUnit(path []string, name string, u *Unit) {
	if !u.Global {
		return
	}
	if c.p.component {
		c.add(at(path, "global"), "global: is for the units of a project; the units of a component are global when the unit that uses it is")
		return
	}
	if _, top := c.p.top["units"][name]; !top && !c.p.Global {
		c.add(at(path, "global"), "global unit %s is in a file that only stack %s includes; declare it in %s or a file of include:", name, c.p.StackName, FileName)
	}
	if u.Service != nil {
		c.add(at(path, "global"), "a global unit runs without the daemon of a stack, so it cannot be a service")
	}
	if at := componentReadsStack(u, map[*Project]bool{}); at != nil {
		c.add(append(slices.Clone(path), "uses"), "global unit %s uses a component that reads stack, at %s:%d:%d; a global unit reads nothing per stack", name, at.File, at.Line, at.Column)
	}
}

// componentReadsStack is where the component that u uses, or one that
// its units use, first reads stack, or nil.
func componentReadsStack(u *Unit, seen map[*Project]bool) *Problem {
	if u.Target == nil || u.Target.Kind != TargetComponent || u.Target.Component == nil {
		return nil
	}
	comp := u.Target.Component
	if seen[comp] {
		return nil
	}
	seen[comp] = true
	if comp.readsStack != nil {
		return comp.readsStack
	}
	for _, name := range slices.Sorted(maps.Keys(comp.Units)) {
		if at := componentReadsStack(comp.Units[name], seen); at != nil {
			return at
		}
	}
	return nil
}

// globalTask checks a task with global: true of the project: outside what
// one stack includes, with no trigger but after, needing and calling only
// global tasks, and bringing up only global units.
func (c *checker) globalTask(path []string, name string, t *Task) {
	if !t.Global || c.p.component {
		return
	}
	if _, top := c.p.top["tasks"][name]; !top && !c.p.Global {
		c.add(at(path, "global"), "global task %s is in a file that only stack %s includes; declare it in %s or a file of include:", name, c.p.StackName, FileName)
	}
	if t.On != nil {
		for kind, set := range map[string]bool{"schedule": len(t.On.Schedule) > 0, "every": len(t.On.Every) > 0,
			"webhook": len(t.On.Webhook) > 0, "watch": len(t.On.Watch) > 0} {
			if set {
				c.add(at(path, "on", kind), "a global task runs after other runs only, through after:; %s runs in the daemon of a stack", kind)
			}
		}
	}
	ps := c.p.stackParts()
	for i, n := range t.Needs {
		if ps.tasks[n] || c.p.Tasks[n] != nil && !c.p.Tasks[n].Global {
			c.add(at(path, "needs", strconv.Itoa(i)), "global task %s needs %s, which is per stack; a global task needs only global tasks", name, n)
		}
	}
	c.globalSteps(at(path, "steps"), name, t.Steps)
}

// globalSteps checks the steps of global task name: task: calls only
// global tasks, and plan, up, and destroy name only global units.
func (c *checker) globalSteps(path []string, name string, steps []*Step) {
	for i, s := range steps {
		sp := at(path, strconv.Itoa(i))
		if s.Task != "" {
			if dep := c.p.Tasks[s.Task]; dep != nil && !dep.Global {
				c.add(at(sp, "task"), "global task %s calls task %s, which is per stack; a global task calls only global tasks", name, s.Task)
			}
			if slices.Contains([]string{"plan", "up", "destroy"}, s.Task) {
				for j, pat := range s.Units {
					segs := ParseAddress(pat)
					if len(segs) == 0 || strings.Contains(segs[0].Unit, "*") || strings.Contains(pat, "${{") {
						continue
					}
					if u := c.p.Units[segs[0].Unit]; u != nil && !u.Global {
						c.add(at(sp, "units", strconv.Itoa(j)), "global task %s names unit %s, which is per stack; a global task reaches only global units", name, segs[0].Unit)
					}
				}
			}
		}
		c.globalSteps(at(sp, "block"), name, s.Block)
		c.globalSteps(at(sp, "rescue"), name, s.Rescue)
		c.globalSteps(at(sp, "always"), name, s.Always)
	}
}

// afterInit checks after: {task: init}: init is followed once it
// succeeds, as a project that does not read cannot run a task, and it
// touches no unit.
func (c *checker) afterInit(path []string, a *After) {
	if c.since(at(path, "task"), "after: {task: init}", "1.1") {
		return
	}
	if a.Outcome != "" && a.Outcome != "success" {
		c.add(at(path, "outcome"), "init is followed only once it succeeds, since a project that does not read cannot run a task; leave outcome: out")
	}
	if a.Unit != "" {
		c.add(at(path, "unit"), "init touches no unit; leave unit: out")
	}
}
