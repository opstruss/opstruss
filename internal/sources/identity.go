// Package sources resolves and verifies project dependencies and their cache.
package sources

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const Repository = "github.com/opstruss/catalog"

// Identity is immutable source provenance, independent of its cache location.
type Identity struct {
	Repository string `json:"repository" yaml:"repository"`
	Path       string `json:"path" yaml:"path"`
	Ref        string `json:"ref" yaml:"ref"`
	Commit     string `json:"commit,omitempty" yaml:"commit,omitempty"`
	Hash       string `json:"hash,omitempty" yaml:"hash,omitempty"`
}

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Parse accepts only the official repository and a safe Git ref.
func Parse(s string) (Identity, error) {
	var id Identity
	addr, ref, ok := strings.Cut(s, "@")
	if !ok || !refPattern.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return id, fmt.Errorf("a remote source is %s[/<path>]@<ref>", Repository)
	}
	if addr != Repository && !strings.HasPrefix(addr, Repository+"/") {
		return id, fmt.Errorf("automatic sources are restricted to %s; prepare other repositories as local paths", Repository)
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(addr, Repository), "/")
	if sub == "" {
		sub = "."
	}
	if !safePath(sub) {
		return id, fmt.Errorf("invalid source path %q", sub)
	}
	return Identity{Repository: Repository, Path: sub, Ref: ref}, nil
}

func safePath(p string) bool {
	return utf8.ValidString(p) && p != "" && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, "/.git/") && !strings.HasSuffix(p, "/.git") && p != ".git" && !strings.HasPrefix(p, ".git/")
}

func (id Identity) Key() string {
	s := id.Repository
	if id.Path != "." {
		s += "/" + id.Path
	}
	return s + "@" + id.Ref
}

func (id Identity) Group() string { return id.Repository + "@" + id.Ref }

func (id Identity) Validate() error {
	x, err := Parse(id.Key())
	if err != nil {
		return err
	}
	if x.Repository != id.Repository || x.Path != id.Path || x.Ref != id.Ref || !commitPattern.MatchString(id.Commit) {
		return fmt.Errorf("invalid pinned source %s", id.Key())
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(id.Hash, "h1:"))
	if err != nil || !strings.HasPrefix(id.Hash, "h1:") || len(b) != sha256.Size || base64.StdEncoding.EncodeToString(b) != strings.TrimPrefix(id.Hash, "h1:") {
		return fmt.Errorf("invalid hash for %s", id.Key())
	}
	return nil
}

// Within checks both the lexical path and every symlink before code is read.
func Within(root, name string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	name, err = filepath.Abs(name)
	if err != nil {
		return "", err
	}
	check := func(p string) bool {
		r, e := filepath.Rel(root, p)
		return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
	}
	if !check(name) {
		return "", fmt.Errorf("source path %s escapes repository root", name)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("source symlink %s escapes repository root", name)
	}
	return real, nil
}

type treeEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
}

func treeHash(entries []treeEntry) string {
	slices.SortFunc(entries, func(a, b treeEntry) int { return strings.Compare(a.Path, b.Path) })
	rows := make([][3]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, [3]string{e.Path, e.Mode, e.Object})
	}
	b, _ := json.Marshal(rows)
	sum := sha256.Sum256(b)
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:])
}

func payloadHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// verifyTree checks the complete snapshot, including unexpected entries.
func verifyTree(root string, entries []treeEntry, expected string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o555 {
		return fmt.Errorf("unsafe source snapshot directory")
	}
	want := map[string]treeEntry{}
	for _, e := range entries {
		want[e.Path] = e
	}
	var actual []treeEntry
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm() != 0o555 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				return fmt.Errorf("source directory mode mismatch: %s", rel)
			}
			if e, ok := want[rel]; ok && e.Mode == "160000" {
				children, err := os.ReadDir(p)
				if err != nil {
					return err
				}
				if len(children) != 0 {
					return fmt.Errorf("populated gitlink %s", rel)
				}
				actual = append(actual, e)
				return filepath.SkipDir
			}
			prefix := rel + "/"
			if !slices.ContainsFunc(entries, func(e treeEntry) bool { return strings.HasPrefix(e.Path, prefix) }) {
				return fmt.Errorf("unexpected directory %s", rel)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var mode string
		var b []byte
		switch {
		case info.Mode().IsRegular():
			mode = "100644"
			if info.Mode().Perm()&0o111 != 0 {
				mode = "100755"
			}
			if (info.Mode().Perm() != 0o444 && info.Mode().Perm() != 0o555) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				return fmt.Errorf("writable or unsafe file %s", rel)
			}
			b, err = os.ReadFile(p)
		case info.Mode()&os.ModeSymlink != 0:
			mode = "120000"
			var target string
			target, err = os.Readlink(p)
			b = []byte(target)
			if err == nil && filepath.IsAbs(target) {
				return fmt.Errorf("absolute symlink %s", rel)
			}
			if err == nil {
				_, err = Within(root, p)
			}
		default:
			return fmt.Errorf("unsupported entry %s", rel)
		}
		if err != nil {
			return err
		}
		actual = append(actual, treeEntry{rel, mode, payloadHash(b)})
		return nil
	})
	if err != nil {
		return err
	}
	if treeHash(actual) != expected {
		return fmt.Errorf("source tree hash mismatch")
	}
	return nil
}
