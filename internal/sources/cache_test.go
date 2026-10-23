package sources

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		if err := removeEntry(home); err != nil {
			t.Error(err)
		}
	})
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "fixture@example.invalid")
	run("config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo", "tool"), []byte("#!/bin/sh\necho v1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tool", filepath.Join(dir, "demo", "link")); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "fixture")
	commit := run("rev-parse", "HEAD")
	run("tag", "v1")
	shim := t.TempDir()
	// Only the harness maps the production URL to a local repository.
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
ls-remote)
  test "$3" = https://github.com/opstruss/plugins.git
  shift 3
  exec '%s' -c protocol.file.allow=always ls-remote --exit-code '%s' "$@"
  ;;
fetch)
  test "$4" = https://github.com/opstruss/plugins.git
  exec '%s' -c protocol.file.allow=always fetch --no-tags --depth=1 '%s' "$5"
  ;;
*) exec '%s' "$@" ;;
esac
`, git, dir, git, dir, git)
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, commit
}

func TestGitCacheIntegrityOfflineAndRepair(t *testing.T) {
	_, commit := gitFixture(t)
	ctx := context.Background()
	cache, err := UserCache()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := Parse(Repository + "/demo@v1")
	resolved, err := cache.ResolveRef(ctx, id)
	if err != nil || resolved != commit {
		t.Fatalf("resolve: %s %v", resolved, err)
	}
	id.Commit = resolved
	id, root, lease, err := cache.Git(ctx, id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	// A fixed fixture hash also locks the byte encoding, executable mode and symlink payload.
	t.Log("fixture hash", id.Hash)
	if id.Hash == "" {
		t.Fatal("missing hash")
	}
	t.Setenv("PATH", t.TempDir())
	_, same, l, err := cache.Git(ctx, id, false, false)
	if err != nil || same != root {
		t.Fatalf("warm offline: %s %v", same, err)
	}
	l.Close()
	if err := os.Chmod(filepath.Join(root, "README.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("tamper"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := cache.Git(ctx, id, false, false); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("corrupt cache: %v", err)
	}
	if _, _, _, err := cache.Git(ctx, id, true, false); err == nil {
		t.Fatal("offline repair succeeded")
	}
	if err := removeEntry(root); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := cache.Git(ctx, id, false, false); err == nil {
		t.Fatal("cold offline succeeded")
	}
}

func TestGitHashModesLinksAndWholeTree(t *testing.T) {
	dir, commit := gitFixture(t)
	ctx := context.Background()
	cache, _ := UserCache()
	id, _ := Parse(Repository + "/demo@v1")
	id.Commit = commit
	id, root, l, err := cache.Git(ctx, id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	mutations := []struct {
		name   string
		change func() error
	}{
		{"mode", func() error { return os.Chmod(filepath.Join(root, "demo", "tool"), 0o444) }},
		{"link", func() error {
			if err := os.Chmod(filepath.Join(root, "demo"), 0o755); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(root, "demo", "link")); err != nil {
				return err
			}
			return os.Symlink("../README.md", filepath.Join(root, "demo", "link"))
		}},
		{"extra", func() error {
			if err := os.Chmod(root, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "extra"), nil, 0o444)
		}},
		{"missing", func() error {
			if err := os.Chmod(root, 0o755); err != nil {
				return err
			}
			return os.Remove(filepath.Join(root, "README.md"))
		}},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.change(); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := cache.Git(ctx, id, false, false); err == nil {
				t.Fatal("tamper accepted")
			}
			_, _, lease, err := cache.Git(ctx, id, true, false)
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
		})
	}
	// Changing a sibling outside demo changes its source hash.
	git, _ := exec.LookPath("git")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "v2"}} {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	cmd := exec.Command(git, "rev-parse", "HEAD")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	next := id
	next.Commit = strings.TrimSpace(string(b))
	next.Hash = ""
	next, _, l, err = cache.Git(ctx, next, false, false)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if next.Hash == id.Hash {
		t.Fatal("sibling README did not affect hash")
	}
}

func TestCacheConcurrentPruneAndLease(t *testing.T) {
	_, commit := gitFixture(t)
	cache, _ := UserCache()
	id, _ := Parse(Repository + "/demo@v1")
	id.Commit = commit
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, _, lease, err := cache.Git(ctx, id, false, false)
			if err == nil {
				lease.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id, root, lease, err := cache.Git(ctx, id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	key := cache.gitKey(id)
	meta, err := cache.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.Used = time.Now().Add(-31 * 24 * time.Hour)
	if err := atomicJSON(cache.metadata(key), meta); err != nil {
		t.Fatal(err)
	}
	removed, err := cache.Prune(ctx)
	if err != nil || len(removed) != 0 {
		t.Fatalf("pruned active: %v %v", removed, err)
	}
	lease.Close()
	removed, err = cache.Prune(ctx)
	if err != nil || len(removed) != 1 {
		t.Fatalf("did not prune old: %v %v", removed, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatal("snapshot remains")
	}
}

func TestSymlinksGitlinksAndGitConfiguration(t *testing.T) {
	dir, commit := gitFixture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "GIT_") {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	// A gitlink binds only its recorded commit; neither its URL nor its objects are fetched.
	run("update-index", "--add", "--cacheinfo", "160000,"+commit+",submodule")
	run("commit", "-qm", "gitlink")
	id, _ := Parse(Repository + "/demo@main")
	id.Commit = run("rev-parse", "HEAD")
	// User Git configuration and repository/object overrides must not influence materialization.
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("[url \"https://invalid.example/\"]\n insteadOf = https://github.com/\n[filter \"evil\"]\n smudge = touch must-not-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_DIR", "/missing/repository")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/missing/objects")
	t.Setenv("GIT_TEMPLATE_DIR", "/missing/template")
	cache, _ := UserCache()
	ctx := context.Background()
	id, root, l, err := cache.Git(ctx, id, false, false)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	entries, err := os.ReadDir(filepath.Join(root, "submodule"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("gitlink: %v %v", entries, err)
	}
	if err := os.Chmod(filepath.Join(root, "submodule"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "submodule", "extra"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := cache.Git(ctx, id, false, false); err == nil {
		t.Fatal("populated gitlink accepted")
	}
	t.Setenv("GIT_DIR", "")
	t.Setenv("GIT_OBJECT_DIRECTORY", "")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_TEMPLATE_DIR", "")
	for _, target := range []string{"../../outside", "/etc/hosts", "missing", "bad"} {
		if err := os.Symlink(target, filepath.Join(dir, "demo", "bad")); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-qm", "unsafe symlink")
		bad := id
		bad.Commit = run("rev-parse", "HEAD")
		bad.Hash = ""
		if _, _, _, err := cache.Git(ctx, bad, false, false); err == nil {
			t.Fatalf("accepted symlink %q", target)
		}
		if err := os.Remove(filepath.Join(dir, "demo", "bad")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneRejectsSymlinkDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	cache := &Cache{Root: root}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "git")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Prune(context.Background()); err == nil {
		t.Fatal("prune accepted symlink directory")
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "untouched" {
		t.Fatalf("outside changed: %s %v", b, err)
	}
}

func TestLocksCancelAndProtectActiveEntries(t *testing.T) {
	cache := &Cache{Root: t.TempDir()}
	ctx := context.Background()
	shared, err := cache.lock(ctx, "entry", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if l, err := cache.lock(ctx, "entry", true, false); err == nil {
		l.Close()
		t.Fatal("exclusive acquired while shared")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if l, err := cache.lock(cancelled, "entry", true, true); err == nil {
		l.Close()
		t.Fatal("cancel did not stop lock wait")
	}
	shared.Close()
	exclusive, err := cache.lock(ctx, "entry", true, false)
	if err != nil {
		t.Fatal(err)
	}
	exclusive.Close()
}
