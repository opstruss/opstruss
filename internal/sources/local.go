package sources

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LocalHash preserves the original path/content hash of local working trees.
// A symlink to a regular file counts with the content of that file.
func LocalHash(dir string) (string, error) {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	h, err := hashFiles(dir, []string{"."})
	if err != nil {
		return "", fmt.Errorf("plugin %s: %w", dir, err)
	}
	return h, nil
}

// hashFiles hashes the regular files under paths in base, and each symlink
// to one with the content of its target, as "<sha256>  <path>" lines by
// path from base. Directories named .git, other symlinks, and special files
// are left out.
func hashFiles(base string, paths []string) (string, error) {
	lines := map[string]string{}
	for _, start := range paths {
		err := filepath.WalkDir(filepath.Join(base, filepath.FromSlash(start)), func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == ".git":
				return filepath.SkipDir
			case d.Type()&fs.ModeSymlink != 0:
				// A link to a directory, a dangling link, or a cycle.
				if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
					return nil
				}
			case !d.Type().IsRegular():
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			lines[rel] = fmt.Sprintf("%s  %s\n", payloadHash(b), rel)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(slices.Sorted(maps.Values(lines)), "")))
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}
