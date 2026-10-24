package sources

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func localHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := LocalHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLocalHashGolden(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"opstruss.plugin.yaml": "name: p\n",
		"up.sh":                "#!/bin/sh\necho up\n",
		"lib/common.sh":        "x=1\n",
		".git/config":          "ignored\n",
	})
	if err := os.Chmod(filepath.Join(dir, "up.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := localHash(t, dir); got != "h1:zdPo+j4EKxbMfW62/Yh4LdHrGyWvachdtJswRW7bpMU=" {
		t.Fatalf("hash = %s", got)
	}
}

func TestLocalHashSymlinks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "plugin")
	writeFiles(t, root, map[string]string{"plugin/opstruss.plugin.yaml": "name: p\n", "shared/up.sh": "v1\n", "shared/dir/x": ""})
	plain := localHash(t, dir)
	for _, link := range [][2]string{{"../shared/dir", "dir"}, {"missing", "dangling"}, {"loop", "loop"}} {
		if err := os.Symlink(link[0], filepath.Join(dir, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	if got := localHash(t, dir); got != plain {
		t.Fatalf("links to a directory, dangling, and in a cycle changed the hash: %s", got)
	}
	if err := os.Symlink("../shared/up.sh", filepath.Join(dir, "up.sh")); err != nil {
		t.Fatal(err)
	}
	linked := localHash(t, dir)
	if linked == plain {
		t.Fatal("a link to a file left the hash")
	}
	writeFiles(t, root, map[string]string{"shared/up.sh": "v2\n"})
	if localHash(t, dir) == linked {
		t.Fatal("a change of a linked file left the hash")
	}
	if err := os.Symlink("plugin", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if localHash(t, filepath.Join(root, "alias")) != localHash(t, dir) {
		t.Fatal("a plugin directory that is a link hashes otherwise than its target")
	}
}
