package model

import (
	"cmp"
	"maps"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/opstruss/opstruss/internal/schema"
)

// PluginFile is the file that marks a plugin.
const PluginFile = "opstruss.plugin.yaml"

// readPlugin reads opstruss.plugin.yaml in dir and checks it; messages name
// the file relative to the project in projectDir.
func readPlugin(projectDir, dir string) (*Plugin, Problems, error) {
	rel, err := filepath.Rel(projectDir, filepath.Join(dir, PluginFile))
	if err != nil {
		return nil, nil, err
	}
	doc, probs, err := readDocument(projectDir, rel, schema.Plugin)
	if err != nil || len(probs) > 0 {
		return nil, probs, err
	}
	pl := &Plugin{}
	if err := decodeValue(doc.value, pl); err != nil {
		return nil, nil, err
	}
	c := &checker{plugin: pl, doc: doc}
	c.pluginRules()
	slices.SortStableFunc(c.probs, func(a, b Problem) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Column, b.Column))
	})
	return pl, c.probs, nil
}

// pluginRules checks what the schema of a plugin cannot: a plugin asks
// for variables and never lets them through itself, its standard tasks and
// hooks come with up, and its tasks are leaves, since chains, approval,
// and the policies of a run belong to the project.
func (c *checker) pluginRules() {
	pl := c.plugin
	if pl.Level != "" && newerLevel(pl.Level, Level) {
		c.add([]string{"opstruss"}, "this file is for OpsTruss %s or newer; this OpsTruss reads files up to %s", pl.Level, Level)
	}
	for i, name := range pl.Env {
		if ReservedEnv(name) {
			c.add([]string{"env", strconv.Itoa(i)}, "OPSTRUSS_ and INPUT_ variables come from OpsTruss; a plugin cannot ask for them")
		}
	}
	if pl.Bin != nil {
		c.add([]string{"bin"}, "not supported yet")
	}
	if pl.Backend != nil {
		c.add([]string{"backend"}, "plugin backends are not supported yet")
	}
	unit := pl.Tasks["up"] != nil
	c.pluginInputs([]string{"inputs"}, pl.Inputs)
	for _, name := range slices.Sorted(maps.Keys(pl.Inputs)) {
		if unit && (name == "units" || name == "force") {
			c.add([]string{"inputs", name}, "%s is an argument of the tasks of a unit (--%s); name the input otherwise", name, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pl.Tasks)) {
		t, path := pl.Tasks[name], []string{"tasks", name}
		switch {
		case name == "migrate":
			c.add(path, "migrate is reserved for a hook of a later OpsTruss")
		case !unit && (slices.Contains(StandardTasks, name) || slices.Contains(Hooks, name)):
			c.add(path, "%s runs only through a unit, and a plugin is a unit only when it has up; give the task another name", name)
		}
		if unit && (name == "plan" || name == "up" || name == "down") && t.Run == nil {
			// The record keeps the program of down to take the unit down
			// without the plugin's file, and up and plan are its peers.
			c.add(path, "%s of a plugin runs one program, written as run", name)
		}
		c.pluginInputs(at(path, "inputs"), t.Inputs)
		vars := map[string]string{}
		for _, in := range slices.Sorted(maps.Keys(pl.Inputs)) {
			vars[InputEnv(in)] = in
		}
		for _, in := range slices.Sorted(maps.Keys(t.Inputs)) {
			switch other, dup := vars[InputEnv(in)]; {
			case pl.Inputs[in] != nil:
				c.add(at(path, "inputs", in), "%s is an input of the plugin already; a task adds inputs of other names", in)
			case dup:
				c.add(at(path, "inputs", in), "inputs %s and %s are both %s for the program; rename one", other, in, InputEnv(in))
			case unit && (in == "units" || (in == "force" && (name == "up" || name == "down"))):
				c.add(at(path, "inputs", in), "%s is an argument of the tasks of a unit (--%s); name the input otherwise", in, in)
			}
		}
		for k, set := range map[string]bool{"needs": len(t.Needs) > 0, "concurrency": t.Concurrency != nil, "internal": t.Internal} {
			if set {
				c.add(at(path, k), "%s is for the tasks of a project; a plugin task is one operation", k)
			}
		}
		if t.Approval == "auto" {
			c.add(at(path, "approval"), "approval: auto is for the project to choose; a plugin cannot approve its own changes")
		}
		c.task(path, t, false)
		c.taskRefs(path, t)
	}
}

// pluginInputs rejects env: on inputs: code from elsewhere reads no
// variable of the operator that the project did not let through.
func (c *checker) pluginInputs(path []string, inputs map[string]*Input) {
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		in, ip := inputs[name], at(path, name)
		if in.Env != "" {
			c.add(at(ip, "env"), "env: of an input is for the files of the project; a plugin gets values through with:")
		}
		if in.Items != nil {
			c.pluginInputs(ip, map[string]*Input{"items": in.Items})
		}
		c.pluginInputs(at(ip, "properties"), in.Properties)
	}
}
