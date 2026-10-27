package model

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// Package is what a name in uses: stands for, or a path that a unit uses
// directly: a plugin, a component, a directory of components and plugins,
// or a built-in task under another name.
type Package struct {
	Source     *sources.Identity
	SourceRoot string
	BinDirs    []string
	Catalog    *Catalog // the catalog that holds the directory; nil outside one

	Name       string  // the name in uses:, or the path that a unit wrote
	Decl       *Uses   // the declaration, with what an alias takes from the one it names; nil for a path in a unit
	Dir        string  // the directory of a plugin, component, or components
	Plugin     *Plugin // opstruss.plugin.yaml, when the directory has one
	Component  bool    // the directory is a component: it has opstruss.yaml and no plugin
	Components []string
	Plugins    []string // the plugins in its plugins/, for a directory that is not a plugin
	Builtin    string   // the built-in task that the name stands for

	broken  bool                // its opstruss.plugin.yaml has problems, reported once
	members map[string]*Package // the plugins of Plugins that units use as <name>.<plugin>, read
}

// errBroken is what a name resolves to in a plugin whose file has
// problems: they are reported in the file, not again where it is used.
var errBroken = errors.New("the plugin has problems")

// What a target is.
const (
	TargetBuiltin   = "builtin"   // a built-in task
	TargetTask      = "task"      // a task of a plugin
	TargetProject   = "project"   // a task of the project; only units use one by name
	TargetPlugin    = "plugin"    // a plugin with up, whose tasks are the lifecycle of the unit
	TargetComponent = "component" // a component, in Dir
)

// Target is what a unit's uses:, or a call of a task by name, runs.
type Target struct {
	Name    string // as written
	Kind    string
	Package *Package // nil for a built-in task called by its own name and for a task of the project
	Task    string   // the task it runs: built-in, of the plugin, or of the project
	Dir     string   // the directory of a component

	// Component is the component in Dir, read; nil while its files have
	// problems.
	Component *Project

	project *Task // the task of the project, for TargetProject
}

func localPath(s string) bool { return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") }

// Alias splits a source that names a plugin or component of another
// declaration, as tofu: cat.tofu; ok is false for any other source.
func Alias(source string) (name, pkg string, ok bool) {
	if localPath(source) || strings.HasPrefix(source, "opstruss.") || strings.HasPrefix(source, "github.com/") {
		return "", "", false
	}
	name, pkg, ok = strings.Cut(source, ".")
	return name, pkg, ok && name != "" && pkg != "" && !strings.Contains(pkg, ".")
}

func isAlias(source string) bool {
	_, _, ok := Alias(source)
	return ok
}

// Resolve finds what a call of a task by name runs: the name in uses: when
// there is one, otherwise a built-in task.
func (p *Project) Resolve(name string) (*Target, error) { return p.resolve(name, false) }

// resolve finds what a name stands for, in a unit's uses: when unit is
// set; a name in uses: comes first, then for a unit a task of the project,
// then a built-in task. Paths are read by the checker.
func (p *Project) resolve(name string, unit bool) (*Target, error) {
	parts := strings.Split(name, ".")
	switch {
	case strings.HasPrefix(name, "github.com/"):
		return nil, fmt.Errorf("remote paths are resolved as unit sources")
	case len(parts) > 2:
		return nil, fmt.Errorf("names of three parts or more are reserved; %s has %d", name, len(parts))
	case len(parts) == 2 && parts[0] == "opstruss":
		if Builtin(parts[1]) == nil {
			return nil, fmt.Errorf("there is no built-in task %s", parts[1])
		}
		return &Target{Name: name, Kind: TargetBuiltin, Task: parts[1]}, nil
	case len(parts) == 2:
		pkg := p.Packages[parts[0]]
		if pkg == nil {
			return nil, fmt.Errorf("uses: declares no %s", parts[0])
		}
		return item(name, pkg, parts[1], unit)
	}
	if pkg := p.Packages[name]; pkg != nil {
		if unit {
			return unitTarget(name, pkg)
		}
		if pkg.Builtin != "" {
			return &Target{Name: name, Kind: TargetBuiltin, Package: pkg, Task: pkg.Builtin}, nil
		}
		return nil, fmt.Errorf("%s is not a task; call a task of it, as %s.<task>", name, name)
	}
	if unit && p.Tasks[name] != nil {
		return &Target{Name: name, Kind: TargetProject, Task: name, project: p.Tasks[name]}, nil
	}
	if Builtin(name) != nil {
		return &Target{Name: name, Kind: TargetBuiltin, Task: name}, nil
	}
	if !unit && p.Tasks[name] != nil {
		return nil, fmt.Errorf("%s is a task of this project; call it with task: %s", name, name)
	}
	if unit {
		return nil, fmt.Errorf("there is no plugin, component, or task %s", name)
	}
	return nil, fmt.Errorf("there is no task %s", name)
}

// item finds <package>.<item>: a task of the plugin, or for a unit a
// component in its components/. A unit that uses a plugin in its plugins/
// is read by the checker first.
func item(name string, pkg *Package, it string, unit bool) (*Target, error) {
	if pkg.broken {
		return nil, errBroken
	}
	if pkg.Builtin != "" {
		return nil, fmt.Errorf("%s is the built-in task %s, which has no %s", pkg.Name, pkg.Builtin, it)
	}
	if pkg.Plugin != nil && pkg.Plugin.Tasks[it] != nil {
		switch {
		case it == "watch":
			return nil, fmt.Errorf("%s is a hook that on: watch calls; write watch: {unit: …} for a unit that uses %s", name, pkg.Name)
		case slices.Contains(Hooks, it):
			return nil, fmt.Errorf("%s is a hook that only opstruss %s calls", name, it)
		case slices.Contains(StandardTasks, it) && unit:
			return nil, fmt.Errorf("%s runs through a unit that uses %s; write uses: %s", name, pkg.Name, pkg.Name)
		case slices.Contains(StandardTasks, it):
			return nil, fmt.Errorf("%s runs only through a unit: write uses: %s in a unit, then opstruss %s <unit> or task: %s", name, pkg.Name, it, it)
		}
		return &Target{Name: name, Kind: TargetTask, Package: pkg, Task: it}, nil
	}
	if slices.Contains(pkg.Components, it) {
		if !unit {
			return nil, fmt.Errorf("%s is a component, which units use", name)
		}
		return &Target{Name: name, Kind: TargetComponent, Package: pkg, Dir: filepath.Join(pkg.Dir, "components", it)}, nil
	}
	if slices.Contains(pkg.Plugins, it) {
		return nil, fmt.Errorf("%s is a plugin, which units use; to call its tasks, name it in uses:, as <name>: %s", name, name)
	}
	if pkg.Plugin == nil {
		return nil, fmt.Errorf("%s has no plugin or component %s", pkg.Name, it)
	}
	return nil, fmt.Errorf("%s has no task or component %s", pkg.Name, it)
}

// unitTarget is what a unit that uses pkg itself runs.
func unitTarget(name string, pkg *Package) (*Target, error) {
	switch {
	case pkg.broken:
		return nil, errBroken
	case pkg.Builtin != "":
		return &Target{Name: name, Kind: TargetBuiltin, Package: pkg, Task: pkg.Builtin}, nil
	case pkg.Plugin != nil && pkg.Plugin.Tasks["up"] == nil && strings.Contains(name, "."):
		return nil, fmt.Errorf("plugin %s has no up, so a unit uses one of its tasks: name it in uses:, as <name>: %s, then uses: <name>.<task>", name, name)
	case pkg.Plugin != nil && pkg.Plugin.Tasks["up"] == nil:
		return nil, fmt.Errorf("plugin %s has no up, so a unit uses one of its tasks, as uses: %s.<task>", name, name)
	case pkg.Plugin != nil:
		return &Target{Name: name, Kind: TargetPlugin, Package: pkg}, nil
	case pkg.Component:
		return &Target{Name: name, Kind: TargetComponent, Package: pkg, Dir: pkg.Dir}, nil
	}
	return nil, fmt.Errorf("%s holds components and plugins; a unit uses one, as uses: %s.<name>", name, name)
}

// Inputs returns the inputs that t takes, and false when it takes any.
func (t *Target) Inputs() (map[string]*Input, bool) {
	switch t.Kind {
	case TargetBuiltin:
		if t.Task == "set" {
			return nil, false
		}
		return Builtin(t.Task).Inputs, true
	case TargetTask:
		in := maps.Clone(t.Package.Plugin.Inputs)
		if in == nil {
			in = map[string]*Input{}
		}
		maps.Copy(in, t.Package.Plugin.Tasks[t.Task].Inputs)
		return in, true
	case TargetPlugin:
		return t.Package.Plugin.Inputs, true
	case TargetProject:
		return t.project.Inputs, true
	case TargetComponent:
		if t.Component != nil {
			return t.Component.Inputs, true
		}
	}
	return nil, false
}

// pkg reads the package in dir, once for each directory, and names it.
func (c *checker) pkg(name string, decl *Uses, dir string) (*Package, error) {
	explicitRemote := strings.HasPrefix(name, "github.com/") || decl != nil && strings.HasPrefix(decl.Source, "github.com/")
	if c.p.Source != nil && !explicitRemote {
		real, err := sources.Within(c.p.SourceRoot, dir)
		if err != nil {
			return nil, err
		}
		dir = real
	}
	base, ok := c.pkgs[dir]
	if !ok {
		var err error
		if base, err = c.readPackage(dir); err != nil {
			return nil, err
		}
		c.pkgs[dir] = base
	}
	pkg := *base
	pkg.Name, pkg.Decl = name, decl
	if c.p.Source == nil && !explicitRemote {
		pkg.Source, pkg.SourceRoot = nil, ""
	}
	if c.p.Source != nil && !explicitRemote {
		id := *c.p.Source
		rel, _ := filepath.Rel(c.p.SourceRoot, dir)
		id.Path = filepath.ToSlash(rel)
		pkg.Source, pkg.SourceRoot = &id, c.p.SourceRoot
	}
	return &pkg, nil
}

func (c *checker) readPackage(dir string) (*Package, error) {
	switch info, err := os.Stat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%s does not exist", c.rel(dir))
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%s is not a directory", c.rel(dir))
	}
	owner, root, err := c.p.Sources.Owner(dir)
	if err != nil {
		return nil, err
	}
	pkg := &Package{Dir: dir, Source: owner, SourceRoot: root}
	switch pkg.Catalog, err = c.catalog(dir, root); {
	case errors.Is(err, errBroken):
		pkg.broken = true
	case err != nil:
		return nil, err
	}
	has := func(name string) (bool, error) {
		_, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	// holding lists the directories in sub that hold file.
	holding := func(sub, file string) ([]string, error) {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			isDir := e.IsDir()
			if !isDir && pkg.Source != nil && e.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(filepath.Join(dir, sub, e.Name()))
				if err != nil {
					return nil, err
				}
				isDir = info.IsDir()
			}
			if !isDir {
				continue
			}
			if ok, err := has(filepath.Join(sub, e.Name(), file)); err != nil {
				return nil, err
			} else if ok {
				names = append(names, e.Name())
			}
		}
		return names, nil
	}
	if pkg.Components, err = holding("components", FileName); err != nil {
		return nil, err
	}
	plugin, err := has(PluginFile)
	if err != nil {
		return nil, err
	}
	switch {
	case plugin:
		if pkg.Source != nil {
			if _, err := sources.Within(pkg.SourceRoot, filepath.Join(dir, PluginFile)); err != nil {
				return nil, err
			}
		}
		if cat := pkg.Catalog; cat != nil {
			if err := sources.PackageLinks(cat.Root, cat.Path, cat.Shared); err != nil {
				return nil, err
			}
		}
		b := catalogBound(pkg.Catalog)
		if b == nil && pkg.Source != nil {
			if b, err = repositoryBound(pkg.SourceRoot, dir); err != nil {
				return nil, err
			}
		}
		pl, probs, err := readPlugin(c.p.base, dir, b)
		if err != nil {
			return nil, err
		}
		c.probs = append(c.probs, probs...)
		pkg.Plugin, pkg.broken = pl, pkg.broken || pl == nil || len(probs) > 0
		if !pkg.broken && !c.p.Sources.Options.Discovery {
			for _, name := range slices.Sorted(maps.Keys(pl.Bin)) {
				binary := pl.Bin[name][sources.Platform()]
				if binary == nil {
					return nil, fmt.Errorf("plugin %s: binary %s has no platform %s", pl.Name, name, sources.Platform())
				}
				binDir, lease, err := c.p.Sources.Cache.Binary(c.p.Sources.Context(), name, *binary, c.p.Sources.Options.Repair, false)
				if err != nil {
					return nil, err
				}
				c.p.Sources.AddBinary(name, *binary, lease)
				pkg.BinDirs = append(pkg.BinDirs, binDir)
			}
		}
	default:
		if pkg.Component, err = has(FileName); err != nil {
			return nil, err
		}
		if pkg.Plugins, err = holding("plugins", PluginFile); err != nil {
			return nil, err
		}
		if !pkg.Component && len(pkg.Components) == 0 && len(pkg.Plugins) == 0 {
			return nil, fmt.Errorf("%s has no %s, %s, components/, or plugins/", c.rel(dir), PluginFile, FileName)
		}
	}
	return pkg, nil
}

// inner reads the plugin or component it in sub, plugins or components,
// of the package parent, under name and decl; it comes from where parent
// comes from.
func (c *checker) inner(parent *Package, sub, it, name string, decl *Uses) (*Package, error) {
	dir := filepath.Join(parent.Dir, sub, it)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	base, ok := c.pkgs[dir]
	if !ok {
		var err error
		if base, err = c.readPackage(dir); err != nil {
			return nil, err
		}
		c.pkgs[dir] = base
	}
	pkg := *base
	pkg.Name, pkg.Decl, pkg.members = name, decl, nil
	if parent.Source == nil {
		pkg.Source, pkg.SourceRoot = nil, ""
	}
	return &pkg, nil
}

// member reads <name>.<plugin> for a unit, once for each name: a plugin in
// plugins/ of the declaration, with the declaration's env: and pass:; nil
// when name is no such plugin.
func (c *checker) member(name string) (*Package, error) {
	parent, it, _ := strings.Cut(name, ".")
	pkg := c.p.Packages[parent]
	if pkg == nil || !slices.Contains(pkg.Plugins, it) {
		return nil, nil
	}
	if m := pkg.members[it]; m != nil {
		return m, nil
	}
	m, err := c.inner(pkg, "plugins", it, name, pkg.Decl)
	if err != nil {
		return nil, err
	}
	if pkg.members == nil {
		pkg.members = map[string]*Package{}
	}
	pkg.members[it] = m
	return m, nil
}

// alias reads the declaration name, whose source names a plugin or
// component of another declaration: env: and pass: of that declaration
// come along, and those of the alias go over them, key by key.
func (c *checker) alias(name string, decl *Uses) (*Package, error) {
	from, it, _ := Alias(decl.Source)
	parent, fromDecl := c.p.Packages[from], c.p.Uses[from]
	switch {
	case fromDecl == nil:
		return nil, fmt.Errorf("uses: declares no %s", from)
	case isAlias(fromDecl.Source):
		return nil, fmt.Errorf("%s is an alias itself; name the declaration it names, as %s", from, fromDecl.Source)
	case parent == nil || parent.broken:
		return nil, errBroken
	case parent.Builtin != "":
		return nil, fmt.Errorf("%s is the built-in task %s, which has no %s", from, parent.Builtin, it)
	}
	var sub string
	switch {
	case slices.Contains(parent.Plugins, it):
		sub = "plugins"
	case slices.Contains(parent.Components, it):
		sub = "components"
	case parent.Plugin != nil && parent.Plugin.Tasks[it] != nil:
		return nil, fmt.Errorf("%s is a task; an alias names a plugin or component, and a task is called as %s", decl.Source, decl.Source)
	case parent.Plugin != nil:
		return nil, fmt.Errorf("%s has no component %s", from, it)
	default:
		return nil, fmt.Errorf("%s has no plugin or component %s", from, it)
	}
	env := maps.Clone(fromDecl.Env)
	if env == nil {
		env = Env{}
	}
	maps.Copy(env, decl.Env)
	if len(env) == 0 {
		env = nil
	}
	d := &Uses{Source: decl.Source, With: decl.With, Env: env, Pass: slices.Concat(fromDecl.Pass, decl.Pass)}
	return c.inner(parent, sub, it, name, d)
}

// rel is dir as the project names it: in a remote source by its address,
// otherwise relative to the project.
func (c *checker) rel(dir string) string {
	if c.p.Sources != nil {
		if a := c.p.Sources.Address(dir); a != "" {
			return a
		}
	}
	if r, err := filepath.Rel(c.p.base, dir); err == nil {
		return filepath.ToSlash(r)
	}
	return dir
}

// packageNames checks a package: the tasks, components, and plugins in it
// have names of their own, and no component or plugin is named as a
// standard task or hook, since <name>.<standard task> is never a call.
func (c *checker) packageNames(path []string, pkg *Package) {
	for _, comp := range pkg.Components {
		switch {
		case slices.Contains(StandardTasks, comp) || slices.Contains(Hooks, comp):
			c.add(path, "%s has a component named %s, which is a standard task or hook; give the component another name", pkg.Name, comp)
		case pkg.Plugin != nil && pkg.Plugin.Tasks[comp] != nil:
			c.add(path, "%s has a task and a component named %s; give one another name", pkg.Name, comp)
		case slices.Contains(pkg.Plugins, comp):
			c.add(path, "%s has a plugin and a component named %s; give one another name", pkg.Name, comp)
		}
	}
	for _, pl := range pkg.Plugins {
		if slices.Contains(StandardTasks, pl) || slices.Contains(Hooks, pl) {
			c.add(path, "%s has a plugin named %s, which is a standard task or hook; give the plugin another name", pkg.Name, pl)
		}
	}
}

// declaredWith checks with: of a declaration: it gives inputs of the
// plugin or its tasks, or of the built-in task.
func (c *checker) declaredWith(path []string, pkg *Package) {
	with := pkg.Decl.With
	if len(with) == 0 || pkg.broken {
		return
	}
	if pkg.Plugin == nil && pkg.Builtin == "" {
		c.add(at(path, "with"), "with: of a declaration is for plugins and built-in tasks; a component takes its inputs from the unit that uses it")
		return
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		if !takes(pkg, k) {
			c.add(at(path, "with", k), "%s has no input %s%s", pkg.Name, k, didYouMean(k, inputNames(pkg)))
		}
	}
}

// inputNames are the inputs of the plugin and its tasks, or of the
// built-in task.
func inputNames(pkg *Package) []string {
	if pkg.Builtin != "" {
		return slices.Collect(maps.Keys(Builtin(pkg.Builtin).Inputs))
	}
	names := slices.Collect(maps.Keys(pkg.Plugin.Inputs))
	for _, t := range pkg.Plugin.Tasks {
		names = append(names, slices.Collect(maps.Keys(t.Inputs))...)
	}
	return names
}

// takes says whether the plugin or one of its tasks, or the built-in task,
// has input k.
func takes(pkg *Package, k string) bool {
	if pkg.Builtin != "" {
		return pkg.Builtin == "set" || Builtin(pkg.Builtin).Inputs[k] != nil
	}
	if pkg.Plugin.Inputs[k] != nil {
		return true
	}
	for _, t := range pkg.Plugin.Tasks {
		if t.Inputs[k] != nil {
			return true
		}
	}
	return false
}

// holdsExpr reports whether v holds ${{ anywhere, written or escaped.
func holdsExpr(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, "${{")
	case []any:
		return slices.ContainsFunc(x, holdsExpr)
	case map[string]any:
		for _, e := range x {
			if holdsExpr(e) {
				return true
			}
		}
	}
	return false
}

// with checks the inputs given to t at path: each is one that t takes, and
// each that t requires is given here or by its declaration; what is
// missing is reported at missing.
func (c *checker) with(path, missing []string, t *Target, with map[string]any) {
	inputs, typed := t.Inputs()
	if !typed {
		return
	}
	for _, k := range slices.Sorted(maps.Keys(with)) {
		v, text := with[k].(string)
		switch {
		case inputs[k] == nil:
			c.add(at(path, k), "%s has no input %s%s", t.Name, k, didYouMean(k, slices.Collect(maps.Keys(inputs))))
		case inputs[k].Literal && text && expr.HasExpr(v):
			c.add(at(path, k), "%s is taken as written; ${{ }} is not allowed in it (write $${{ for ${{)", k)
		case holdsExpr(with[k]):
			// Only a run knows the value; it checks it then.
		default:
			// A value as written is checked as a run checks it, as
			// terraform validate checks a variable given a constant.
			if _, err := inputs[k].Conform(with[k]); err != nil {
				c.add(at(path, k), "%v", err)
			} else if t.Kind == TargetBuiltin && t.Task == "kv" && k == "key" && text {
				if err := state.CheckKVName(v); err != nil {
					c.add(at(path, k), "%v", err)
				}
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(inputs)) {
		in := inputs[k]
		if !in.Required || in.Default != nil || in.Env != "" {
			continue
		}
		if _, ok := with[k]; ok {
			continue
		}
		if t.Package != nil && t.Package.Decl != nil {
			if _, ok := t.Package.Decl.With[k]; ok {
				continue
			}
		}
		c.add(missing, "%s needs input %s", t.Name, k)
	}
}

// sourcePackage preserves remote provenance for both declarations and direct uses.
func (c *checker) sourcePackage(name string, decl *Uses, source string) (*Package, error) {
	id, dir, err := c.p.Sources.Resolve(source)
	if err != nil {
		if c.p.Sources.Options.Discovery {
			return &Package{Name: name, Decl: decl, broken: true}, nil
		}
		return nil, err
	}
	pkg, err := c.pkg(name, decl, dir)
	if err != nil {
		return nil, err
	}
	pkg.Source, pkg.SourceRoot = &id, c.p.Sources.Cache.Snapshot(id)
	return pkg, nil
}
