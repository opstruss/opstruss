package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/opstruss/opstruss/internal/sources"
)

// CatalogFile marks the root of a catalog: a repository of plugins and
// components that may use the paths it shares.
const CatalogFile = "opstruss.catalog.yaml"

// catalogFile is what opstruss.catalog.yaml holds.
type catalogFile struct {
	Level    string         `json:"opstruss,omitempty"`
	Shared   []string       `json:"shared,omitempty"`
	Packages []packageEntry `json:"packages"`
}

// packageEntry is an entry of packages: a path or glob, named by its last
// directory, or a name with a path.
type packageEntry struct {
	Name, Path string
	long       bool
}

func (e *packageEntry) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &e.Path)
	}
	var f struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	err := decodeJSON(b, &f)
	e.Name, e.Path, e.long = f.Name, f.Path, true
	return err
}

// Catalog is the catalog that holds a plugin or a component. The hash of
// the package covers its directory and the shared paths, so the relative
// paths in it lead nowhere else.
type Catalog struct {
	Root     string            // the directory of opstruss.catalog.yaml, without symlinks
	Path     string            // the package, relative to Root, with /
	Shared   []string          // relative to Root, with /, without symlinks
	Packages map[string]string // what packages: names, by name, relative to Root, with /, without symlinks; nil without packages:
}

// catalog finds the catalog of the package in dir: for a remote source,
// opstruss.catalog.yaml at the root of its snapshot, otherwise the first
// one from dir up, as go finds go.mod. errBroken means the file has
// problems, which are reported once.
func (c *checker) catalog(dir, sourceRoot string) (*Catalog, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // the caller reports the missing directory
	}
	if err != nil {
		return nil, err
	}
	root := dir
	if sourceRoot != "" {
		if root, err = filepath.EvalSymlinks(sourceRoot); err != nil {
			return nil, err
		}
	}
	for {
		_, err := os.Stat(filepath.Join(root, CatalogFile))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if sourceRoot != "" || filepath.Dir(root) == root {
			return nil, nil
		}
		root = filepath.Dir(root)
	}
	cat, ok := c.cats[root]
	if !ok {
		if cat, err = c.readCatalog(root); err != nil {
			return nil, err
		}
		c.cats[root] = cat
	}
	if cat == nil {
		return nil, errBroken
	}
	rel, _ := filepath.Rel(root, dir)
	return &Catalog{Root: root, Path: filepath.ToSlash(rel), Shared: cat.Shared, Packages: cat.Packages}, nil
}

// readCatalog reads opstruss.catalog.yaml at root, which is without
// symlinks: nil when it has problems, which go to the checker.
func (c *checker) readCatalog(root string) (*Catalog, error) {
	// Messages name the file from the project, which may lie behind a symlink.
	base, err := filepath.EvalSymlinks(c.p.base)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, filepath.Join(root, CatalogFile))
	if err != nil {
		return nil, err
	}
	doc, probs, err := readDocument(base, rel, schema.Catalog, c.p.Sources)
	var pp Problems
	switch {
	case errors.As(err, &pp):
		probs = pp
	case err != nil:
		return nil, err
	}
	cat := &Catalog{Root: root, Shared: []string{}}
	if len(probs) == 0 {
		var f catalogFile
		if err := decodeValue(doc.value, &f); err != nil {
			return nil, err
		}
		for i, s := range f.Shared {
			at := []string{"shared", strconv.Itoa(i)}
			if path.IsAbs(s) || strings.ContainsAny(s, "*?[") || path.Clean(s) != s || s == "." || s == ".." || strings.HasPrefix(s, "../") {
				probs = append(probs, doc.problem(at, "a shared path is relative to the root of the catalog, without .., globs, or a leading /"))
				continue
			}
			real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(s)))
			rel, _ := filepath.Rel(root, real)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				probs = append(probs, doc.problem(at, "%s does not exist", s))
			case err != nil:
				return nil, err
			case !filepath.IsLocal(rel):
				probs = append(probs, doc.problem(at, "%s leads outside the catalog through a symlink", s))
			default:
				cat.Shared = append(cat.Shared, filepath.ToSlash(rel))
			}
		}
		if len(probs) == 0 {
			var err error
			if cat.Packages, probs, err = readPackages(doc, root, cat.Shared, f.Packages); err != nil {
				return nil, err
			}
		}
	}
	if len(probs) > 0 {
		c.probs = append(c.probs, probs...)
		return nil, nil
	}
	return cat, nil
}

// packageName matches the name of a package of a catalog: a name that
// <catalog>.<name> can mean, so no standard task or hook.
var packageName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func isPackageName(name string) bool {
	return packageName.MatchString(name) && !slices.Contains(StandardTasks, name) && !slices.Contains(Hooks, name)
}

// manifest is the manifest in dir: opstruss.plugin.yaml, which comes first,
// or opstruss.yaml; "" when dir is no directory or has neither.
func manifest(dir string) (string, error) {
	switch info, err := os.Stat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", nil
	}
	for _, name := range []string{PluginFile, FileName} {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// readPackages reads packages: of the catalog at root, whose shared paths
// are read: nil without it, as every subdirectory with a manifest is then
// a package by its name. A path is named once and a name names one path,
// no package lies in another, and none lies in a shared path or holds one.
func readPackages(doc *document, root string, shared []string, entries []packageEntry) (map[string]string, Problems, error) {
	rootManifest, err := manifest(root)
	if err != nil {
		return nil, nil, err
	}
	var probs Problems
	switch {
	case entries != nil && rootManifest != "":
		return nil, Problems{doc.problem([]string{"packages"}, "this root has %s, so it is a package itself and lists no packages", rootManifest)}, nil
	case rootManifest != "":
		return nil, nil, nil
	case entries == nil:
		// Every subdirectory with a manifest is a package, so a shared
		// path is none of them and lies in none.
		for i, s := range shared {
			top, _, _ := strings.Cut(s, "/")
			if !isPackageName(top) {
				continue
			}
			m, err := manifest(filepath.Join(root, top))
			switch {
			case err != nil:
				return nil, nil, err
			case m != "" && top == s:
				probs = append(probs, doc.problem([]string{"shared", strconv.Itoa(i)}, "%s has %s, so it is a package; a shared path is no package", s, m))
			case m != "":
				probs = append(probs, doc.problem([]string{"shared", strconv.Itoa(i)}, "%s is in the package %s; a shared path lies outside the packages", s, top))
			}
		}
		return nil, probs, nil
	}
	packages := map[string]string{}
	byPath, byName := map[string]string{}, map[string]string{} // the entry of each path, the path of each name
	for i, e := range entries {
		pos := []string{"packages", strconv.Itoa(i)}
		where := pos
		if e.long {
			where = at(pos, "path")
		}
		s, glob := e.Path, strings.ContainsAny(e.Path, "*?[")
		switch {
		case strings.HasPrefix(s, "!"):
			probs = append(probs, doc.problem(where, "a leading ! is reserved; list the packages to keep"))
			continue
		case strings.Contains(s, "**"):
			probs = append(probs, doc.problem(where, "** is reserved; * matches the directories of one level, as tools/*"))
			continue
		case path.IsAbs(s) || path.Clean(s) != s || s == "." || s == ".." || strings.HasPrefix(s, "../"):
			msg := "a package path is relative to the root of the catalog, without ./, .., or a leading or trailing /"
			if fix := strings.TrimPrefix(path.Clean(s), "/"); filepath.IsLocal(fix) {
				msg += "; write " + fix
			}
			probs = append(probs, doc.problem(where, "%s", msg))
			continue
		case glob && e.long:
			probs = append(probs, doc.problem(where, "the path of a named package is one directory, without a glob"))
			continue
		}
		var paths []string
		if glob {
			if _, err := path.Match(s, ""); err != nil {
				probs = append(probs, doc.problem(where, "%s is not a glob: %v", s, err))
				continue
			}
			matches, err := fs.Glob(os.DirFS(root), s)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range matches {
				if mf, err := manifest(filepath.Join(root, filepath.FromSlash(m))); err != nil {
					return nil, nil, err
				} else if mf != "" {
					paths = append(paths, m)
				}
			}
			if len(paths) == 0 {
				probs = append(probs, doc.problem(where, "%s matches no directory with %s or %s", s, PluginFile, FileName))
				continue
			}
		} else {
			switch mf, err := manifest(filepath.Join(root, filepath.FromSlash(s))); {
			case err != nil:
				return nil, nil, err
			case mf == "":
				probs = append(probs, doc.problem(where, "%s is no directory with %s or %s", s, PluginFile, FileName))
				continue
			}
			paths = []string{s}
		}
		entry := fmt.Sprintf("packages[%d]", i)
		for _, p := range paths {
			real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				return nil, nil, err
			}
			rel, _ := filepath.Rel(root, real)
			if !filepath.IsLocal(rel) {
				probs = append(probs, doc.problem(where, "%s leads outside the catalog through a symlink", p))
				continue
			}
			rel = filepath.ToSlash(rel)
			name := e.Name
			switch {
			case !e.long:
				if name = path.Base(p); !isPackageName(name) {
					probs = append(probs, doc.problem(pos, "%s is not a package name: lowercase letters, digits, - and _, starting with a letter, and no standard task or hook; name it with {name: <name>, path: %s}", name, p))
					continue
				}
			case !isPackageName(name):
				probs = append(probs, doc.problem(at(pos, "name"), "%s is a standard task or hook, which <catalog>.%s never means; give the package another name", name, name))
				continue
			}
			if first, ok := byPath[rel]; ok {
				probs = append(probs, doc.problem(pos, "%s is a package of %s already; a path has one name", p, first))
				continue
			}
			if first, ok := byName[name]; ok {
				probs = append(probs, doc.problem(pos, "%s names both %s and %s; give one another name", name, first, p))
				continue
			}
			nested := false
			for _, other := range slices.Sorted(maps.Keys(byPath)) {
				switch {
				case strings.HasPrefix(rel, other+"/"):
					probs = append(probs, doc.problem(pos, "%s is in the package %s of %s; a package holds no other package", p, other, byPath[other]))
				case strings.HasPrefix(other, rel+"/"):
					probs = append(probs, doc.problem(pos, "%s holds the package %s of %s; a package holds no other package", p, other, byPath[other]))
				default:
					continue
				}
				nested = true
			}
			if nested {
				continue
			}
			for _, sh := range shared {
				switch {
				case rel == sh || strings.HasPrefix(rel, sh+"/"):
					probs = append(probs, doc.problem(pos, "%s is in the shared path %s; a package is not shared", p, sh))
				case strings.HasPrefix(sh, rel+"/"):
					probs = append(probs, doc.problem(pos, "%s holds the shared path %s; a shared path lies outside the packages", p, sh))
				}
			}
			byPath[rel], byName[name] = entry, p
			packages[name] = rel
		}
	}
	return packages, probs, nil
}

// bound is where the relative paths of a package may lead: in a catalog,
// the package and the shared paths, which its hash covers; in another
// remote repository, its root.
type bound struct {
	root, dir string   // without symlinks
	paths     []string // relative to root
	what      string   // the paths, for messages
}

// catalogBound is the bound of the package of cat, nil outside a catalog.
func catalogBound(cat *Catalog) *bound {
	if cat == nil {
		return nil
	}
	return &bound{root: cat.Root, dir: filepath.Join(cat.Root, filepath.FromSlash(cat.Path)),
		paths: append([]string{cat.Path}, cat.Shared...), what: "its package and the shared paths of its catalog"}
}

// repositoryBound is the bound of the package in dir of a remote
// repository outside a catalog: the root of its snapshot.
func repositoryBound(sourceRoot, dir string) (*bound, error) {
	root, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	return &bound{root: root, dir: dir, paths: []string{"."}, what: "its repository"}, nil
}

// allows reports whether name, relative to the package, stays in b.
func (b *bound) allows(name string) (bool, error) {
	if b == nil {
		return true, nil
	}
	return sources.Inside(b.root, b.paths, filepath.Join(b.dir, filepath.FromSlash(name)))
}

// programs calls fn with each program that the tasks of the plugin name
// by a path relative to its directory, and the path where it is written.
func (pl *Plugin) programs(fn func(path []string, prog string)) {
	check := func(path []string, cmd *Command) {
		if cmd == nil || len(cmd.Argv) == 0 {
			return
		}
		prog := cmd.Argv[0]
		if strings.Contains(prog, "/") && !filepath.IsAbs(prog) && !expr.HasExpr(prog) {
			fn(at(path, "0"), prog)
		}
	}
	var steps func(path []string, ss []*Step)
	steps = func(path []string, ss []*Step) {
		for i, s := range ss {
			p := at(path, strconv.Itoa(i))
			check(at(p, "run"), s.Run)
			steps(at(p, "block"), s.Block)
			steps(at(p, "rescue"), s.Rescue)
			steps(at(p, "always"), s.Always)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pl.Tasks)) {
		t, path := pl.Tasks[name], []string{"tasks", name}
		check(at(path, "run"), t.Run)
		steps(at(path, "steps"), t.Steps)
		if t.Stop != nil {
			check(at(path, "stop", "run"), t.Stop.Run)
		}
	}
}

// RelativePrograms are the programs that the tasks of the plugin name by a
// path relative to its directory, each once.
func (pl *Plugin) RelativePrograms() []string {
	var progs []string
	pl.programs(func(_ []string, prog string) {
		if !slices.Contains(progs, prog) {
			progs = append(progs, prog)
		}
	})
	return progs
}
