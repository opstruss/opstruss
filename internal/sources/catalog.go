package sources

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// PackageHash is the hash of a package of the catalog in the snapshot of
// id: the h1: hash of the entries under the package directory and the
// shared paths, by path from the root of the repository. The paths are
// relative to that root, with symlinks resolved.
func (c *Cache) PackageHash(id Identity, pkg string, shared []string) (string, error) {
	m, err := c.readMeta(c.gitKey(id))
	if err != nil {
		return "", fmt.Errorf("%s: source metadata unavailable: %w", id.Key(), err)
	}
	if treeHash(m.Entries) != id.Hash {
		return "", fmt.Errorf("%s: cache integrity: source tree hash mismatch; run opstruss plugins fetch to repair", id.Key())
	}
	paths := append([]string{pkg}, shared...)
	var entries []treeEntry
	for _, e := range m.Entries {
		if slices.ContainsFunc(paths, func(p string) bool { return p == "." || e.Path == p || strings.HasPrefix(e.Path, p+"/") }) {
			entries = append(entries, e)
		}
	}
	return treeHash(entries), nil
}

// PackageLinks checks that each symlink under the package and the shared
// paths of the catalog at root resolves inside one of them, since the hash
// of the package covers nothing else. A dangling link is left to the hash,
// which leaves it out. The paths are relative to root, with symlinks
// resolved.
func PackageLinks(root, pkg string, shared []string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	paths := append([]string{pkg}, shared...)
	for _, start := range paths {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(start)), func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == ".git":
				return filepath.SkipDir
			case d.Type()&fs.ModeSymlink == 0:
				return nil
			}
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil
			}
			if !inside(root, paths, real) {
				rel, _ := filepath.Rel(root, p)
				return fmt.Errorf("symlink %s leads outside its package and the shared paths of the catalog", filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Inside reports whether name lies in one of paths, which are relative to
// root and free of symlinks, both by its path and through each symlink; a
// name that does not exist is judged by its path.
func Inside(root string, paths []string, name string) (bool, error) {
	name, err := filepath.Abs(name)
	if err != nil || !inside(root, paths, name) {
		return false, err
	}
	real, err := filepath.EvalSymlinks(name)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return false, err
	}
	return inside(root, paths, real), nil
}

func inside(root string, paths []string, name string) bool {
	return slices.ContainsFunc(paths, func(p string) bool {
		rel, err := filepath.Rel(filepath.Join(root, filepath.FromSlash(p)), name)
		return err == nil && filepath.IsLocal(rel)
	})
}
