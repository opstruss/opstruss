package model

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// Package is what a name in uses: stands for, or a path that a unit uses
// directly: a plugin, a component, a catalog of them, or a built-in task
// under another name.
type Package struct {
	Source     *sources.Identity
	SourceRoot string
	BinDirs    []string
	Catalog    *Catalog // the catalog that holds the directory; nil outside one

	Name      string  // the name in uses:, or the path that a unit wrote
	Decl      *Uses   // the declaration, with what an alias takes from the one it names; nil for a path in a unit
	Dir       string  // the directory of a plugin, component, or catalog
	Plugin    *Plugin // opstruss.plugin.yaml, when the directory has one
	Component bool    // the directory is a component: it has opstruss.yaml and no plugin
	Builtin   string  // the built-in task that the name stands for

	broken  bool                // its opstruss.plugin.yaml or opstruss.catalog.yaml has problems, reported once
	members map[string]*Package // the packages of a catalog that units use as <name>.<package>, read
}

// IsCatalog reports whether the package is a catalog: a directory with no
// manifest of its own, whose packages units use as <name>.<package>.
func (pkg *Package) IsCatalog() bool {
	return pkg.Dir != "" && pkg.Plugin == nil && !pkg.Component && !pkg.broken
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

// Alias splits a source that names a package of the catalog of another
// declaration, as tofu: ops.tofu; ok is false for any other source.
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
	case len(parts) > 2 && unit:
		return nil, fmt.Errorf("names of three parts or more are reserved; to use a task of a package of %s, declare the package in uses:, as %s: %s.%s, and use %s.%s",
			parts[0], parts[1], parts[0], parts[1], parts[1], strings.Join(parts[2:], "."))
	case len(parts) > 2:
		return nil, fmt.Errorf("names of three parts or more are reserved; to call a task of a package of %s, declare the package in uses:, as %s: %s.%s, and call %s.%s",
			parts[0], parts[1], parts[0], parts[1], parts[1], strings.Join(parts[2:], "."))
	case len(parts) == 2 && parts[0] == "opstruss":
		if Builtin(parts[1]) == nil {
			return nil, fmt.Errorf("there is no built-in task %s%s", parts[1], expr.DidYouMean(parts[1], slices.Collect(maps.Keys(builtins()))))
		}
		return &Target{Name: name, Kind: TargetBuiltin, Task: parts[1]}, nil
	case len(parts) == 2:
		pkg := p.Packages[parts[0]]
		if pkg == nil {
			return nil, fmt.Errorf("uses: declares no %s%s", parts[0], expr.DidYouMean(parts[0], slices.Collect(maps.Keys(p.Packages))))
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
	names := slices.Concat(slices.Collect(maps.Keys(p.Packages)), slices.Collect(maps.Keys(builtins())))
	if unit {
		names = append(names, slices.Collect(maps.Keys(p.Tasks))...)
		return nil, fmt.Errorf("there is no plugin, component, or task %s%s", name, expr.DidYouMean(name, names))
	}
	return nil, fmt.Errorf("there is no task %s%s", name, expr.DidYouMean(name, names))
}

// item finds <package>.<item>: a task of the plugin. A unit that uses a
// package of a catalog is read by the checker first.
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
	switch {
	case pkg.Plugin != nil:
		return nil, fmt.Errorf("%s has no task %s%s", pkg.Name, it, expr.DidYouMean(it, slices.Collect(maps.Keys(pkg.Plugin.Tasks))))
	case pkg.Component:
		return nil, fmt.Errorf("%s is a component, which holds no %s; a unit uses it as uses: %s", pkg.Name, it, pkg.Name)
	}
	return nil, fmt.Errorf("%s is a catalog, whose packages units use; to call a task of %s, name it in uses:, as <name>: %s, and call <name>.<task>", pkg.Name, name, name)
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
	case localPath(name) || strings.HasPrefix(name, "github.com/"):
		return nil, fmt.Errorf("%s has no %s or %s", name, PluginFile, FileName)
	}
	return nil, fmt.Errorf("%s is a catalog, as it has no %s or %s; a unit uses one of its packages, as uses: %s.<package>", name, PluginFile, FileName, name)
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
	m, err := manifest(dir)
	if err != nil {
		return nil, err
	}
	switch m {
	case PluginFile:
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
		pl, probs, err := readPlugin(c.p.base, dir, b, c.p.Sources)
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
	case FileName:
		pkg.Component = true
	}
	return pkg, nil
}

// inner reads the package in dir of the catalog parent, under name and
// decl; it comes from where parent comes from.
func (c *checker) inner(parent *Package, dir, name string, decl *Uses) (*Package, error) {
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

// member reads <name>.<package> for a unit, once for each name: a package
// of the catalog that the declaration stands for, with the declaration's
// env: and pass:; nil when the declaration is no catalog.
func (c *checker) member(name string) (*Package, error) {
	parent, it, _ := strings.Cut(name, ".")
	pkg := c.p.Packages[parent]
	if pkg == nil || !pkg.IsCatalog() || it == "" || strings.Contains(it, ".") {
		return nil, nil
	}
	if m := pkg.members[it]; m != nil {
		return m, nil
	}
	dir, err := c.packageDir(pkg, it)
	if err != nil {
		return nil, err
	}
	m, err := c.inner(pkg, dir, name, pkg.Decl)
	if err != nil {
		return nil, err
	}
	if pkg.members == nil {
		pkg.members = map[string]*Package{}
	}
	pkg.members[it] = m
	return m, nil
}

// alias reads the declaration name, whose source names a package of the
// catalog of another declaration: env: and pass: of that declaration come
// along, and those of the alias go over them, key by key.
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
	switch {
	case parent.Plugin != nil && parent.Plugin.Tasks[it] != nil:
		return nil, fmt.Errorf("%s is a task; an alias names a package of a catalog, and a task is called as %s", decl.Source, decl.Source)
	case parent.Plugin != nil:
		return nil, fmt.Errorf("%s is a plugin, which holds no packages; an alias names a package of a catalog", from)
	case parent.Component:
		return nil, fmt.Errorf("%s is a component, which holds no packages; an alias names a package of a catalog", from)
	}
	dir, err := c.packageDir(parent, it)
	if err != nil {
		return nil, err
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
	return c.inner(parent, dir, name, d)
}

// packageDir is the directory of the package it of the catalog pkg: the
// one that packages: of its opstruss.catalog.yaml names it, or else its
// subdirectory it, which needs a manifest. Only that directory is read;
// the others only for the message when there is none.
func (c *checker) packageDir(pkg *Package, it string) (string, error) {
	if cat := pkg.Catalog; cat != nil && cat.Path == "." && cat.Packages != nil {
		if p, ok := cat.Packages[it]; ok {
			return filepath.Join(cat.Root, filepath.FromSlash(p)), nil
		}
	} else if isPackageName(it) {
		dir := filepath.Join(pkg.Dir, it)
		if m, err := manifest(dir); err != nil {
			return "", err
		} else if m != "" {
			return dir, nil
		}
	}
	return "", c.noPackage(pkg, it)
}

// packagesOf is the directory of each package of the catalog pkg, by name,
// as packageDir finds them: those that packages: of its
// opstruss.catalog.yaml names, or else each subdirectory with a manifest.
func packagesOf(pkg *Package) (map[string]string, error) {
	out := map[string]string{}
	if cat := pkg.Catalog; cat != nil && cat.Path == "." && cat.Packages != nil {
		for name, p := range cat.Packages {
			out[name] = filepath.Join(cat.Root, filepath.FromSlash(p))
		}
		return out, nil
	}
	entries, err := os.ReadDir(pkg.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !isPackageName(e.Name()) {
			continue
		}
		dir := filepath.Join(pkg.Dir, e.Name())
		if m, err := manifest(dir); err != nil {
			return nil, err
		} else if m != "" {
			out[e.Name()] = dir
		}
	}
	return out, nil
}

// noPackage says that the catalog pkg has no package it, with the packages
// it has and a deeper directory it with a manifest, which packages: may
// name.
func (c *checker) noPackage(pkg *Package, it string) error {
	cat := pkg.Catalog
	listed := cat != nil && cat.Path == "." && cat.Packages != nil
	pkgs, err := packagesOf(pkg)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(pkgs))
	msg := fmt.Sprintf("%s has no package %s", pkg.Name, it)
	// A package deeper down is named only by packages:.
	var deeper string
	err = filepath.WalkDir(pkg.Dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || deeper != "":
			return filepath.SkipAll
		case !d.IsDir():
			return nil
		case p != pkg.Dir && strings.HasPrefix(d.Name(), "."):
			return filepath.SkipDir
		case d.Name() != it || !listed && p == filepath.Join(pkg.Dir, it):
			return nil
		}
		if m, _ := manifest(p); m != "" {
			rel, _ := filepath.Rel(pkg.Dir, p)
			deeper = filepath.ToSlash(rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	near := expr.Nearest(it, names)
	switch {
	case deeper != "" && listed:
		msg += fmt.Sprintf("; %s has a manifest: add it to packages: in %s", deeper, CatalogFile)
	case deeper != "":
		msg += fmt.Sprintf("; %s has a manifest: add packages: [%s/*] to %s", deeper, path.Dir(deeper), CatalogFile)
	case near != "":
		msg += "; did you mean " + near + "?"
	case len(names) > 0:
		msg += "; its packages are " + strings.Join(names, ", ")
	case !listed:
		msg += fmt.Sprintf("; %s has no subdirectory with %s or %s", c.rel(pkg.Dir), PluginFile, FileName)
	}
	return errors.New(msg)
}

// packageNamed is the directory of the package that the path of id names
// in the catalog at the root of its snapshot, as <catalog>.<name> does.
func (c *checker) packageNamed(id sources.Identity, root string) (string, error) {
	whole := id
	whole.Path = "."
	cat, err := c.pkg(whole.Key(), nil, root)
	switch {
	case err != nil:
		return "", err
	case cat.broken:
		return "", errBroken
	case cat.Plugin != nil:
		return "", fmt.Errorf("%s is a plugin, which holds no packages; use it as %s", whole.Key(), whole.Key())
	case cat.Component:
		return "", fmt.Errorf("%s is a component, which holds no packages; use it as %s", whole.Key(), whole.Key())
	case strings.Contains(id.Path, "/"):
		msg := fmt.Sprintf("%s names a directory; a source in a catalog names one of its packages, as %s/<package>@%s", id.Key(), id.Repository, id.Ref)
		if ct := cat.Catalog; ct != nil {
			for name, p := range ct.Packages {
				if p == id.Path {
					named := id
					named.Path = name
					msg += "; write " + named.Key()
				}
			}
		}
		return "", errors.New(msg)
	}
	return c.packageDir(cat, id.Path)
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
			c.add(at(path, "with", k), "%s has no input %s%s", pkg.Name, k, expr.DidYouMean(k, inputNames(pkg)))
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
			c.add(at(path, k), "%s has no input %s%s", t.Name, k, expr.DidYouMean(k, slices.Collect(maps.Keys(inputs))))
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

// sourcePackage preserves remote provenance for both declarations and direct
// uses. In a repository with opstruss.catalog.yaml at its root, the path
// after the repository is the name of a package, so the directories of the
// catalog stay its own, as the exports of package.json keep the files of a
// package: a package keeps its address when it moves.
func (c *checker) sourcePackage(name string, decl *Uses, source string) (*Package, error) {
	id, dir, err := c.p.Sources.Resolve(source)
	if err != nil {
		if c.p.Sources.Options.Discovery {
			return &Package{Name: name, Decl: decl, broken: true}, nil
		}
		return nil, err
	}
	root := c.p.Sources.Cache.Snapshot(id)
	named := false
	if id.Path != "." {
		switch _, err := os.Stat(filepath.Join(root, CatalogFile)); {
		case err == nil:
			if dir, err = c.packageNamed(id, root); errors.Is(err, errBroken) {
				return &Package{Name: name, Decl: decl, broken: true}, nil
			} else if err != nil {
				return nil, err
			}
			named = true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	pkg, err := c.pkg(name, decl, dir)
	if err != nil {
		return nil, err
	}
	// A package found by its name has the identity of its directory, as
	// <catalog>.<name> gives it.
	if !named || pkg.Source == nil {
		pkg.Source = &id
	}
	pkg.SourceRoot = root
	return pkg, nil
}
