package sources

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitFixture writes files into the fixture repository and commits them.
func commitFixture(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	writeFiles(t, dir, files)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "change"}, {"rev-parse", "HEAD"}} {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err = cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return strings.TrimSpace(string(out))
}

func TestPackageHashCoversPackageAndShared(t *testing.T) {
	dir, _ := gitFixture(t)
	ctx := context.Background()
	cache, _ := UserCache()
	hash := func(commit string) (Identity, string) {
		t.Helper()
		id, _ := Parse(Repository + "/demo@v1")
		id.Commit = commit
		id, _, l, err := cache.Git(ctx, id, false, false)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		h, err := cache.PackageHash(id, "demo", []string{"lib"})
		if err != nil {
			t.Fatal(err)
		}
		return id, h
	}
	first, h := hash(commitFixture(t, dir, map[string]string{"lib/opstruss.sh": "v1\n", "other/up.sh": "v1\n"}))
	if whole, err := cache.PackageHash(first, ".", nil); err != nil || whole != first.Hash {
		t.Fatalf("whole tree = %s, %v; want %s", whole, err, first.Hash)
	}
	id, same := hash(commitFixture(t, dir, map[string]string{"README.md": "v2\n", "other/up.sh": "v2\n"}))
	if id.Hash == first.Hash || same != h {
		t.Fatalf("a change outside the package: tree %s → %s, package %s → %s", first.Hash, id.Hash, h, same)
	}
	_, shared := hash(commitFixture(t, dir, map[string]string{"lib/opstruss.sh": "v2\n"}))
	if shared == h {
		t.Fatal("a change of a shared path left the package hash")
	}
	_, own := hash(commitFixture(t, dir, map[string]string{"demo/tool": "v2\n"}))
	if own == shared {
		t.Fatal("a change in the package left its hash")
	}
}

func TestLocalPackageHashAndLinks(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"README.md":                      "v1\n",
		"lib/opstruss.sh":                "v1\n",
		"plugins/a/opstruss.plugin.yaml": "name: a\n",
		"plugins/a/up.sh":                "v1\n",
		"plugins/b/up.sh":                "v1\n",
	})
	if err := os.Symlink("../../lib/opstruss.sh", filepath.Join(root, "plugins/a/lib.sh")); err != nil {
		t.Fatal(err)
	}
	hash := func() string {
		t.Helper()
		h, err := LocalPackageHash(root, "plugins/a", []string{"lib"})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := hash()
	writeFiles(t, root, map[string]string{"README.md": "v2\n", "plugins/b/up.sh": "v2\n"})
	if hash() != h {
		t.Fatal("a change outside the package changed its hash")
	}
	writeFiles(t, root, map[string]string{"lib/opstruss.sh": "v2\n"})
	if next := hash(); next == h {
		t.Fatal("a change of a shared path left the package hash")
	} else {
		h = next
	}
	writeFiles(t, root, map[string]string{"plugins/a/up.sh": "v2\n"})
	if hash() == h {
		t.Fatal("a change in the package left its hash")
	}

	if err := PackageLinks(root, "plugins/a", []string{"lib"}); err != nil {
		t.Fatalf("a link to a shared path: %v", err)
	}
	if err := os.Symlink("../b/up.sh", filepath.Join(root, "plugins/a/b.sh")); err != nil {
		t.Fatal(err)
	}
	if err := PackageLinks(root, "plugins/a", []string{"lib"}); err == nil || !strings.Contains(err.Error(), "symlink plugins/a/b.sh leads outside") {
		t.Fatalf("a link to another package: %v", err)
	}
}

func TestInside(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"lib/x.sh": "", "plugins/a/up.sh": "", "plugins/b/up.sh": ""})
	if err := os.Symlink("../b", filepath.Join(root, "plugins/a/b")); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "plugins/a")
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"up.sh", true},
		{"missing.sh", true},
		{"../../lib/x.sh", true},
		{"../b/up.sh", false},
		{"b/up.sh", false},
		{"../../../x", false},
	} {
		got, err := Inside(root, []string{"plugins/a", "lib"}, filepath.Join(pkg, tt.name))
		if err != nil || got != tt.want {
			t.Errorf("%s: %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}
