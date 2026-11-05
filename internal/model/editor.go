package model

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/schema"
)

// EditorSchemaFile is the JSON Schema of the files of a project for
// editors, in the directory of the project: the schema of opstruss.yaml in
// which each with: and each call of a task takes the inputs of what it
// names. init names it in the first line of opstruss.yaml, and each load
// of the project writes it again when it changes.
const EditorSchemaFile = ".opstruss/schema.json"

// accepts is what a name in the files takes: its inputs, or any input when
// open; with the description of what the name stands for.
type accepts struct {
	inputs      map[string]*Input
	open        bool
	description string
}

// merge joins what one name takes in two places, such as two stacks or a
// project and a component that both declare it: each input of either, and
// any value for an input that they describe differently, so that a file
// that loads is never marked wrong.
func (t accepts) merge(o accepts) accepts {
	out := accepts{open: t.open || o.open}
	if t.description == o.description {
		out.description = t.description
	}
	if out.open {
		return out
	}
	out.inputs = maps.Clone(t.inputs)
	if out.inputs == nil {
		out.inputs = map[string]*Input{}
	}
	for k, in := range o.inputs {
		if have, ok := out.inputs[k]; ok && jsonText(have) != jsonText(in) {
			in = &Input{}
		}
		out.inputs[k] = in
	}
	return out
}

// editorNames are what the files of a project name, by where they name it.
type editorNames struct {
	units  map[string]accepts // uses: of a unit
	calls  map[string]accepts // the key of a call of a task
	tasks  map[string]accepts // task: of a step, and the task of a trigger
	decls  map[string]accepts // with: of a declaration in uses:
	stacks accepts            // with: of a stack: the inputs of the project
}

func (n *editorNames) put(m map[string]accepts, name string, t accepts) {
	if have, ok := m[name]; ok {
		t = have.merge(t)
	}
	m[name] = t
}

// add adds what the files of p name. Tasks come also from the components
// that its units use, which give their instances tasks; names come only
// from files in the project, local, which are edited with it.
func (n *editorNames) add(p *Project, root, local bool, seen map[*Project]bool) {
	if seen[p] {
		return
	}
	seen[p] = true
	if root {
		n.stacks = n.stacks.merge(accepts{inputs: p.Inputs})
	}
	for name, t := range p.Tasks {
		n.put(n.tasks, name, accepts{inputs: t.Inputs, description: t.Description})
	}
	for _, u := range p.Units {
		for name, t := range u.Tasks {
			n.put(n.tasks, name, accepts{inputs: t.Inputs, description: t.Description})
		}
	}
	if local {
		n.names(p)
	}
	for _, u := range p.Units {
		if t := u.Target; t != nil && t.Kind == TargetComponent && t.Component != nil {
			n.add(t.Component, false, local && t.Package != nil && t.Package.Source == nil, seen)
		}
	}
}

// names adds what a unit's uses:, a call of a task, and a declaration name
// in the files of p, as resolve and the checker find them.
func (n *editorNames) names(p *Project) {
	candidates := map[string]bool{}
	for name := range builtins() {
		candidates[name], candidates["opstruss."+name] = true, true
	}
	for name := range p.Tasks {
		candidates[name] = true
	}
	for name, pkg := range p.Packages {
		candidates[name] = true
		if pkg.Plugin != nil {
			for task := range pkg.Plugin.Tasks {
				candidates[name+"."+task] = true
			}
		}
	}
	for name := range candidates {
		if t, err := p.resolve(name, true); err == nil {
			if tk, ok := p.acceptsOf(t); ok {
				n.put(n.units, name, tk)
			}
		}
		if t, err := p.resolve(name, false); err == nil {
			if tk, ok := p.acceptsOf(t); ok {
				n.put(n.calls, name, tk)
			}
		}
	}
	for name, pkg := range p.Packages {
		if !pkg.IsCatalog() {
			continue
		}
		members, err := packagesOf(pkg)
		if err != nil {
			continue
		}
		for m, dir := range members {
			if t := p.memberTarget(dir); t != nil {
				if tk, ok := p.acceptsOf(t); ok {
					n.put(n.units, name+"."+m, tk)
				}
			}
		}
	}
	// A path or source that a unit names itself.
	for _, u := range p.Units {
		if u.Target != nil {
			if tk, ok := p.acceptsOf(u.Target); ok {
				n.put(n.units, u.Uses, tk)
			}
		}
	}
	for name := range p.Uses {
		pkg := p.Packages[name]
		switch {
		case pkg == nil || pkg.broken:
		case pkg.Builtin != "":
			if tk, ok := p.acceptsOf(&Target{Kind: TargetBuiltin, Task: pkg.Builtin}); ok {
				n.put(n.decls, name, tk)
			}
		case pkg.Plugin != nil:
			// with: of a declaration gives an input of the plugin or of
			// any of its tasks.
			tk := accepts{inputs: pkg.Plugin.Inputs}
			for _, t := range pkg.Plugin.Tasks {
				tk = tk.merge(accepts{inputs: t.Inputs})
			}
			n.put(n.decls, name, tk)
		}
	}
}

// memberTarget is what a unit that uses the package of a catalog in dir
// runs; nil when no unit can use it or its file has problems.
func (p *Project) memberTarget(dir string) *Target {
	switch m, _ := manifest(dir); m {
	case PluginFile:
		pl, probs, err := readPlugin(p.base, dir, nil, p.Sources)
		if err == nil && pl != nil && len(probs) == 0 && pl.Tasks["up"] != nil {
			return &Target{Kind: TargetPlugin, Package: &Package{Plugin: pl}}
		}
	case FileName:
		return &Target{Kind: TargetComponent, Dir: dir}
	}
	return nil
}

// acceptsOf is what t takes; false when it is a component whose files have
// problems.
func (p *Project) acceptsOf(t *Target) (accepts, bool) {
	if t.Kind == TargetComponent && t.Component == nil {
		c, probs, err := readComponent(p.base, t.Dir, p.StackName, p.Sources, nil, "", nil)
		if err != nil || len(probs) > 0 {
			return accepts{}, false
		}
		read := *t
		read.Component, t = c, &read
	}
	in, typed := t.Inputs()
	tk := accepts{inputs: in, open: !typed}
	switch t.Kind {
	case TargetBuiltin:
		tk.description = Builtin(t.Task).Description
	case TargetTask:
		tk.description = t.Package.Plugin.Tasks[t.Task].Description
	case TargetPlugin:
		tk.description = t.Package.Plugin.Description
	case TargetProject:
		tk.description = t.project.Description
	case TargetComponent:
		tk.description = t.Component.Description
	}
	return tk, true
}

// EditorSchema is the JSON Schema of opstruss.yaml for editors, for ps, the
// project read for one or more of its stacks: each with: and each call of
// a task takes the inputs of what it names, a unit's uses: offers the
// names, and nothing is required, since a declaration, env:, or a default
// may give an input. It accepts what validate accepts, which stays the
// check.
func EditorSchema(ps ...*Project) ([]byte, error) {
	n := &editorNames{units: map[string]accepts{}, calls: map[string]accepts{}, tasks: map[string]accepts{}, decls: map[string]accepts{}}
	seen := map[*Project]bool{}
	for _, p := range ps {
		n.add(p, true, true, seen)
	}
	raw, err := schema.Raw(schema.Project)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["$comment"] = "Written by OpsTruss from the files of this project and what they use; each command that reads the project writes it again."
	defs := doc["$defs"].(map[string]any)
	e := &editorDefs{defs: defs, names: map[string]string{}}
	object := func(name string) map[string]any { return defs[name].(map[string]any) }
	properties := func(s map[string]any) map[string]any { return s["properties"].(map[string]any) }

	unit := object("unit")
	var conds, names, descriptions []any
	for _, name := range slices.Sorted(maps.Keys(n.units)) {
		t := n.units[name]
		if !t.open {
			conds = append(conds, withIf("uses", name, e.with(t)))
		}
		// The names of the project, not built-in tasks or paths.
		if Builtin(name) == nil && !strings.HasPrefix(name, "opstruss.") && !strings.Contains(name, "/") {
			names, descriptions = append(names, name), append(descriptions, t.description)
		}
	}
	if len(conds) > 0 {
		unit["allOf"] = conds
	}
	if len(names) > 0 {
		properties(unit)["uses"].(map[string]any)["anyOf"] = []any{
			map[string]any{"enum": names, "enumDescriptions": descriptions}, map[string]any{}}
	}

	// A step and a task call any task by name; a secret and a watch take
	// fewer, so they keep the call of the base schema.
	for _, def := range []string{"step", "task"} {
		props := properties(object(def))
		for _, name := range slices.Sorted(maps.Keys(n.calls)) {
			if _, ok := props[name]; ok {
				continue
			}
			t := n.calls[name]
			ref := map[string]any{"$ref": "#/$defs/call"}
			if !t.open {
				ref = e.with(t)
			}
			call := map[string]any{"allOf": []any{ref}}
			if t.description != "" {
				call["description"] = t.description
			}
			props[name] = call
		}
	}

	decls := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(n.decls)) {
		if t := n.decls[name]; !t.open {
			decls[name] = map[string]any{"allOf": []any{
				map[string]any{"$ref": "#/$defs/uses/additionalProperties"},
				map[string]any{"properties": map[string]any{"with": e.with(t)}},
			}}
		}
	}
	if len(decls) > 0 {
		object("uses")["properties"] = decls
	}

	properties(object("stack"))["with"].(map[string]any)["allOf"] = []any{e.with(n.stacks)}

	conds = nil
	tasks := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(n.tasks)) {
		t := n.tasks[name]
		if t.open {
			continue
		}
		with := e.with(t)
		conds = append(conds, withIf("task", name, with))
		// Each trigger of the task, one or a list, gives it inputs.
		trigger := map[string]any{"properties": map[string]any{"with": with}}
		tasks[name] = map[string]any{"allOf": []any{
			map[string]any{"$ref": "#/$defs/task"},
			map[string]any{"properties": map[string]any{"on": map[string]any{"additionalProperties": map[string]any{
				"properties": trigger["properties"], "items": trigger}}}},
		}}
	}
	if len(conds) > 0 {
		object("step")["allOf"] = conds
	}
	if len(tasks) > 0 {
		object("tasks")["properties"] = tasks
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// withIf gives with: the schema with in an object whose key is value. It
// matches value by a pattern, not const: while it completes, the YAML
// language server takes a const or enum that starts with the value
// written as a match, which would give with: the inputs of each name that
// starts with this one.
func withIf(key, value string, with map[string]any) map[string]any {
	is := map[string]any{"type": "string", "pattern": "^" + regexp.QuoteMeta(value) + "$"}
	return map[string]any{
		"if":   map[string]any{"properties": map[string]any{key: is}, "required": []any{key}},
		"then": map[string]any{"properties": map[string]any{"with": with}},
	}
}

// editorDefs holds the schema of each with: once in $defs, by its JSON.
type editorDefs struct {
	defs  map[string]any
	names map[string]string
}

// with refers to the schema of with: for what takes t: its inputs, and
// no other key.
func (e *editorDefs) with(t accepts) map[string]any {
	props := map[string]any{}
	for k, in := range t.inputs {
		props[k] = inputSchema(in)
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	key := jsonText(s)
	name, ok := e.names[key]
	for i := len(e.names) + 1; !ok; i++ {
		if name = "with-" + strconv.Itoa(i); e.defs[name] == nil {
			e.names[key], e.defs[name], ok = name, s, true
		}
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

var (
	// expression is a value that a run knows, and checks then.
	expression = map[string]any{"type": "string", "pattern": `\$\{\{`}
	// null gives an input its default, as Conform does.
	null = map[string]any{"type": "null"}
)

// inputSchema is the JSON Schema of a value of in as written: what
// Conform takes, ${{ }}, or null.
func inputSchema(in *Input) map[string]any {
	s := map[string]any{}
	if in.Description != "" {
		s["description"] = in.Description
	}
	if in.Default != nil && !in.Sensitive {
		s["default"] = in.Default
	}
	typed := map[string]any{}
	if in.Type != "" {
		typed["type"] = in.Type
	}
	switch in.Type {
	case "string":
		if in.Pattern != "" {
			typed["pattern"] = in.Pattern
		}
	case "integer", "number":
		if in.Minimum != nil {
			typed["minimum"] = *in.Minimum
		}
		if in.Maximum != nil {
			typed["maximum"] = *in.Maximum
		}
	case "array":
		if in.Items != nil {
			typed["items"] = inputSchema(in.Items)
		}
	case "object":
		if len(in.Properties) > 0 {
			props := map[string]any{}
			for k, p := range in.Properties {
				props[k] = inputSchema(p)
			}
			typed["properties"], typed["additionalProperties"] = props, false
		}
	}
	if len(in.Enum) > 0 {
		typed["enum"] = in.Enum
	}
	if len(typed) > 0 {
		s["anyOf"] = []any{typed, expression, null}
	}
	return s
}

// WriteEditorSchema writes EditorSchemaFile in the directory of ps, the
// project read for one or more of its stacks, when it changed. It is
// replaced whole, since an editor may be reading it.
func WriteEditorSchema(ps ...*Project) error {
	if len(ps) == 0 {
		return nil
	}
	data, err := EditorSchema(ps...)
	if err != nil {
		return err
	}
	name := filepath.Join(ps[0].Dir, filepath.FromSlash(EditorSchemaFile))
	if old, err := os.ReadFile(name); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".schema-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), name)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
