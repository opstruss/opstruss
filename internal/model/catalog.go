package model

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
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
	Level  string   `json:"opstruss,omitempty"`
	Shared []string `json:"shared,omitempty"`
}

// Catalog is the catalog that holds a plugin or a component. The hash of
// the package covers its directory and the shared paths, so the relative
// paths in it lead nowhere else.
type Catalog struct {
	Root   string   // the directory of opstruss.catalog.yaml, without symlinks
	Path   string   // the package, relative to Root, with /
	Shared []string // relative to Root, with /, without symlinks
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
	return &Catalog{Root: root, Path: filepath.ToSlash(rel), Shared: cat.Shared}, nil
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
	doc, probs, err := readDocument(base, rel, schema.Catalog)
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
	}
	if len(probs) > 0 {
		c.probs = append(c.probs, probs...)
		return nil, nil
	}
	return cat, nil
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
