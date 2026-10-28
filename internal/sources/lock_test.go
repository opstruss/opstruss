package sources

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockTransaction(t *testing.T) {
	_, _ = gitFixture(t)
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Begin(ctx, dir, Options{Locked: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Resolve(Repository + "/demo@v1"); err == nil {
		t.Fatal("locked accepted missing pin")
	}
	s.Close()
	s, err = Begin(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.Resolve(Repository + "/demo@v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, err := os.ReadFile(filepath.Join(dir, LockName))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	s, err = Begin(ctx, dir, Options{Locked: true})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Resolve(id.Key())
	if err != nil || second != id {
		t.Fatalf("pinned offline: %v %v", second, err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	after, _ := os.ReadFile(filepath.Join(dir, LockName))
	if !bytes.Equal(before, after) {
		t.Fatal("warm run rewrote lock")
	}
	for _, b := range []string{"{}\n", "sources: {}\n", "version: 1\n", "version: 1\nsources: null\n", "version: 1\nversion: 1\nsources: {}\n", "version: 1\nsources: {}\n---\nversion: 1\n"} {
		if err := os.WriteFile(filepath.Join(dir, LockName), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLock(dir); err == nil {
			t.Errorf("accepted %q", b)
		}
	}
	// A newer version is refused before its shape is read; a field of the
	// same version that this OpsTruss does not know is ignored.
	for b, want := range map[string]string{
		"version: 2\nformat: later\nsources: []\n": "has version 2, written by a newer OpsTruss",
		"version: 1\nsources: {}\nlater: true\n":   "",
	} {
		if err := os.WriteFile(filepath.Join(dir, LockName), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadLock(dir)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want %q", b, err, want)
		}
	}
}
