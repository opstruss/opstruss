//go:build failpoints

// Package failpoint marks the points where tests stop OpsTruss as a crash
// would, as gofail does in etcd. Only builds with the failpoints tag do
// anything at them.
package failpoint

import (
	"os"
	"strings"
	"syscall"
	"time"
)

// Enabled reports whether this build stops at failpoints.
const Enabled = true

// Hit appends name to the file that OPSTRUSS_FAILPOINT_LOG names, so a test
// can check that every point was reached. When OPSTRUSS_FAILPOINT is
// name=kill, the process dies at once, as kill -9 makes it; name=pause
// waits until the file in OPSTRUSS_FAILPOINT_PAUSE is removed, so a test
// can act at that point.
func Hit(name string) {
	if log := os.Getenv("OPSTRUSS_FAILPOINT_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.WriteString(name + "\n")
			_ = f.Close()
		}
	}
	point, action, _ := strings.Cut(os.Getenv("OPSTRUSS_FAILPOINT"), "=")
	if point != name {
		return
	}
	switch action {
	case "kill":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	case "pause":
		pause := os.Getenv("OPSTRUSS_FAILPOINT_PAUSE")
		_ = os.WriteFile(pause, []byte(name), 0o600)
		for {
			if _, err := os.Stat(pause); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
