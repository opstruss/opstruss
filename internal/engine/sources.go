package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

func (e *engine) outside() *plugin.Run {
	r := plugin.Outside(e.p.Name, e.p.StackName, e.opts.Environ)
	r.SourceFiles, r.RetainSources = e.p.Sources.Files, e.p.Sources.Retain
	r.PluginBins = e.p.PluginBinaries()
	return r
}

func sourceOf(id *sources.Identity) *state.Source {
	if id == nil {
		return nil
	}
	return &state.Source{Uses: id.Key(), Commit: id.Commit, Hash: id.Hash, Remote: id}
}

// sourceContract records every snapshot needed by the already resolved cleanup.
func (e *engine) sourceContract(inst *instance, r *resolved, d *state.Down) {
	if d == nil {
		return
	}
	add := func(id *sources.Identity) {
		if id == nil {
			return
		}
		root := e.p.Sources.Cache.Snapshot(*id)
		if !slices.ContainsFunc(d.Sources, func(s state.SourceLocation) bool { return s.Root == root }) {
			d.Sources = append(d.Sources, state.SourceLocation{Source: *id, Root: root})
		}
	}
	for node := inst.node; node != nil; node = node.Parent {
		add(node.In.Source)
	}
	if t := inst.def.Target; t != nil && t.Package != nil {
		add(t.Package.Source)
	}
	if id, root, err := e.locationOwner(inst, r, r.cwd); err == nil && id != nil {
		add(id)
		d.DirSource = id
		rel, _ := filepath.Rel(root, r.cwd)
		d.Dir = filepath.ToSlash(rel)
	}
	if r.plugin != nil {
		pkg := r.plugin.target.Package
		if pkg.Source != nil {
			d.PluginSource = pkg.Source
			d.Plugin = pkg.Source.Path
		}
		for name, platforms := range pkg.Plugin.Bin {
			if b := platforms[sources.Platform()]; b != nil {
				if d.Binaries == nil {
					d.Binaries = map[string]sources.Binary{}
				}
				d.Binaries[name] = *b
			}
		}
	}
}

// prepareDown restores pins without consulting current definitions or the lock.
// It is shared by planning and execution, including pending/abandoned cleanup.
func (e *engine) prepareDown(ctx context.Context, d *state.Down) (dir, pluginDir string, bins []string, files []*os.File, release func(), err error) {
	var leases []*sources.Lease
	release = func() {
		for _, l := range leases {
			l.Close()
		}
	}
	fail := func(cause error) (string, string, []string, []*os.File, func(), error) {
		release()
		return "", "", nil, nil, func() {}, cause
	}
	if d == nil {
		return "", "", nil, nil, release, nil
	}
	cache := e.p.Sources.Cache
	for _, location := range d.Sources {
		if err := location.Source.Validate(); err != nil {
			return fail(err)
		}
		if cache.Snapshot(location.Source) != location.Root {
			return fail(fmt.Errorf("cleanup source location changed; restore the original cache root %s", location.Root))
		}
		_, _, l, err := cache.Git(ctx, location.Source, false, false)
		if err != nil {
			return fail(err)
		}
		leases = append(leases, l)
	}
	resolve := func(id *sources.Identity, p string) (string, error) {
		if id == nil {
			if filepath.IsAbs(p) {
				return p, nil
			}
			return filepath.Join(e.p.Dir, filepath.FromSlash(p)), nil
		}
		if err := id.Validate(); err != nil {
			return "", err
		}
		if !slices.ContainsFunc(d.Sources, func(s state.SourceLocation) bool {
			return s.Source.Repository == id.Repository && s.Source.Commit == id.Commit && s.Source.Hash == id.Hash
		}) {
			return "", fmt.Errorf("cleanup location has no pinned source")
		}
		if !filepath.IsLocal(p) {
			return "", fmt.Errorf("cleanup path escapes source")
		}
		return sources.Within(cache.Snapshot(*id), filepath.Join(cache.Snapshot(*id), filepath.FromSlash(p)))
	}
	if dir, err = resolve(d.DirSource, d.Dir); err != nil {
		return fail(err)
	}
	if d.Plugin != "" {
		if pluginDir, err = resolve(d.PluginSource, d.Plugin); err != nil {
			return fail(err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Binaries)) {
		b, l, err := cache.Binary(ctx, name, d.Binaries[name], false, false)
		if err != nil {
			return fail(err)
		}
		leases = append(leases, l)
		bins = append(bins, b)
	}
	for _, l := range leases {
		files = append(files, l.File())
	}
	return dir, pluginDir, bins, files, release, nil
}

func (e *engine) checkDownSources(ctx context.Context, rec *state.Record) error {
	contracts := []*state.Down{rec.Down}
	if rec.Pending != nil {
		contracts = append(contracts, rec.Pending.Down)
	}
	for _, a := range rec.Abandoned {
		contracts = append(contracts, a.Down)
	}
	for _, d := range contracts {
		if err := e.checkDown(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) pluginPath(pkg *model.Package) string {
	return strings.Join(append(slices.Clone(pkg.BinDirs), e.path()), string(os.PathListSeparator))
}

func (e *engine) requiredPath(r *resolved) string {
	if r.plugin != nil {
		return e.pluginPath(r.plugin.target.Package)
	}
	if r.task != nil && r.task.target.Package != nil {
		return e.pluginPath(r.task.target.Package)
	}
	return e.path()
}

func downIdentities(d *state.Down) []sources.Identity {
	var ids []sources.Identity
	for _, location := range d.Sources {
		ids = append(ids, location.Source)
	}
	return ids
}

func (e *engine) checkDown(ctx context.Context, d *state.Down) error {
	_, _, _, _, release, err := e.prepareDown(ctx, d)
	if err != nil {
		return err
	}
	release()
	return nil
}

func (e *engine) locationOwner(inst *instance, r *resolved, name string) (*sources.Identity, string, error) {
	ids := []*sources.Identity{inst.node.In.Source}
	if r.plugin != nil {
		ids = append(ids, r.plugin.target.Package.Source)
	}
	if r.task != nil && r.task.target.Package != nil {
		ids = append(ids, r.task.target.Package.Source)
	}
	for _, id := range ids {
		if id == nil {
			continue
		}
		root := e.p.Sources.Cache.Snapshot(*id)
		rel, err := filepath.Rel(root, name)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		real, err := sources.Within(root, name)
		if err != nil {
			return nil, "", err
		}
		rel, _ = filepath.Rel(root, real)
		copy := *id
		copy.Path = filepath.ToSlash(rel)
		return &copy, root, nil
	}
	return nil, "", nil
}
