package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeftLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const backend, key = "loc through plugin", "s/web/state.lock"
	if LeftLock(backend, key, "") {
		t.Fatal("an empty token is left")
	}
	if err := LeaveLock(backend, key, "old"); err != nil {
		t.Fatal(err)
	}
	cleanup := LeftLockCleanup(backend, key)
	if cleanup == nil {
		t.Fatal("cleanup missed the old marker")
	}
	if err := LeaveLock(backend, key, "new"); err != nil {
		t.Fatal(err)
	}
	// A late acknowledgement and a delayed acquisition cleanup can both
	// finish after the new holder has left its marker.
	ForgetLock(backend, key, "old")
	if !LeftLock(backend, key, "new") {
		t.Fatal("unlock cleanup removed the new marker")
	}
	if err := LeaveLock(backend, key, "old"); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if LeftLock(backend, key, "old") || !LeftLock(backend, key, "new") {
		t.Fatal("acquisition cleanup did not keep just the new marker")
	}
	ForgetLock(backend, key, "new")
	files, err := os.ReadDir(filepath.Join(home, ".opstruss", "left-locks"))
	if err != nil || len(files) != 0 {
		t.Fatalf("left-locks holds %d entries, %v; want 0", len(files), err)
	}
}

func TestLeftLockProbe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := LeaveLock("backend", "_probe/1a2b3c4d/unit/state.lock", "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".opstruss", "left-locks")); !os.IsNotExist(err) {
		t.Fatalf("probe created left-locks or stat failed: %v", err)
	}
}
