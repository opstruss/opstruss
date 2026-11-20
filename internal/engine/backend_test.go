package engine

import (
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/plugin"
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
