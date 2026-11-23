package engine

import (
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// idleBackend answers initialize, then every other request with a record,
// by the count of the requests, which ids follow.
const idleBackend = `n=0
while IFS= read -r line; do
	n=$((n + 1))
	case $line in
	*'"initialize"'*) printf '{"jsonrpc":"2.0","id":%d,"result":{"protocol":1,"location":"test:"}}\n' "$n" ;;
	*) printf '{"jsonrpc":"2.0","id":%d,"result":{"data":"","etag":"1"}}\n' "$n" ;;
	esac
done`

func TestBackendIdle(t *testing.T) {
	defer func(d time.Duration) { backendIdle = d }(backendIdle)
	backendIdle = 20 * time.Millisecond
	b := &pluginBackend{name: "test", log: func(string, string) {},
		prog: &plugin.Program{Path: "/bin/sh", Args: []string{"sh", "-c", idleBackend}}}
	defer b.close()
	// The program that starts after one stopped unused stops unused too.
	for i := 1; i <= 2; i++ {
		if _, _, err := b.Get("k"); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			b.mu.Lock()
			running := b.cur != nil
			b.mu.Unlock()
			if !running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("program %d still runs after it sat unused", i)
			}
		}
	}
}

func TestUnlockCommand(t *testing.T) {
	for key, want := range map[string]string{
		"prod/web/state.lock":          "opstruss unlock web --stack prod",
		"prod/web[eu]/state.lock":      "opstruss unlock 'web[eu]' --stack prod",
		"prod/web[it's]/state.lock":    `opstruss unlock 'web[it'\''s]' --stack prod`,
		"prod/dns/zone/state.lock":     "opstruss unlock dns/zone --stack prod",
		"_global/zone/state.lock":      "opstruss unlock zone --global",
		"prod/_kv.lock":                "opstruss unlock _kv --stack prod",
		"prod/_daemon.lock":            "opstruss unlock _daemon --stack prod",
		"_probe/1a2b3c4d/u/state.lock": "",
	} {
		if _, got := unlockCommand(key); got != want {
			t.Errorf("unlockCommand(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestLeftLockBackendIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &pluginBackend{location: "test:", source: "plugins/db", lockSource: "/project-a/plugins/db"}
	b := &pluginBackend{location: "test:", source: "plugins/db", lockSource: "/project-b/plugins/db"}
	lock := &state.LockedError{Key: "prod/web/state.lock", Token: "token"}
	if err := state.LeaveLock(a.lockIdentity(), lock.Key, lock.Token); err != nil {
		t.Fatal(err)
	}
	if stale, left := a.stale(lock.Key, lock); !stale || !left {
		t.Fatal("backend A missed its left lock")
	}
	if stale, _ := b.stale(lock.Key, lock); stale {
		t.Fatal("backend B used backend A's left lock")
	}
}
