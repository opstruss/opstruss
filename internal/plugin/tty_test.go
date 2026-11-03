package plugin

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/model"
)

// buffer is a writer that a stream of a call writes to from its goroutine.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// A program with TTY has a terminal of that size for stdout and stderr,
// which come as one stream, and stdin as it was.
func TestTTY(t *testing.T) {
	r := newRun(t)
	out, errw := &buffer{}, &buffer{}
	res := run(t, r, &Call{TTY: &TTY{Cols: 70, Rows: 20}, Stdout: out, Stderr: errw,
		Command: script(`test -t 1 && test -t 2 && ! test -t 0 && stty size 0<&1 && echo err >&2`)})
	if res.ExitCode != 0 || !strings.Contains(out.String(), "20 70\r\nerr\r\n") || errw.String() != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.ExitCode, out.String(), errw.String())
	}
	if res.Stderr != "" || !strings.Contains(res.Stdout, "20 70") {
		t.Fatalf("result stdout %q, stderr %q", res.Stdout, res.Stderr)
	}
}

// What a program writes just before it ends is read, all of it, before the
// terminal goes.
func TestTTYEnd(t *testing.T) {
	r := newRun(t)
	out := &buffer{}
	res := run(t, r, &Call{TTY: &TTY{Cols: 80, Rows: 24}, Stdout: out,
		Command: script(`head -c 1048576 /dev/zero | tr '\0' x`)})
	if n := strings.Count(out.String(), "x"); res.ExitCode != 0 || n != 1<<20 {
		t.Fatalf("exit %d, %d bytes of %d", res.ExitCode, n, 1<<20)
	}
	for i := range 50 {
		out := &buffer{}
		run(t, r, &Call{TTY: &TTY{Cols: 80, Rows: 24}, Stdout: out, Command: script(`printf 'last line\n'`)})
		if out.String() != "last line\r\n" {
			t.Fatalf("run %d: %q", i, out.String())
		}
	}
}

// What a program leaves in its group stops with it, and the call returns
// once it did.
func TestTTYLeft(t *testing.T) {
	r := newRun(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	began := time.Now()
	res := run(t, r, &Call{TTY: &TTY{Cols: 80, Rows: 24}, Stop: Stop{Signal: syscall.SIGTERM, Grace: time.Second},
		Stdout: &buffer{}, Command: script(`sleep 60 & echo $! > ` + pidFile)})
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	eventually(t, "the background sleep to end", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	if d := time.Since(began); d > 2*time.Second || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "left in its process group") {
		t.Fatalf("after %s, warnings %q", d, res.Warnings)
	}

	// One that left the group and keeps the terminal does not hold the call.
	began = time.Now()
	res = run(t, r, &Call{TTY: &TTY{Cols: 80, Rows: 24}, Stdout: &buffer{},
		Env:     map[string]model.EnvValue{"PLUGIN_HELPER": {Text: "escape"}, "PLUGIN_HELPER_PID": {Text: pidFile + "2"}, "HELPER": {Text: os.Args[0]}},
		Command: script(`"$HELPER" & while ! test -s "$PLUGIN_HELPER_PID"; do sleep 0.01; done`)})
	eventually(t, "the helper's pid", func() bool {
		data, _ := os.ReadFile(pidFile + "2")
		pid, _ = strconv.Atoi(string(data))
		return pid > 0
	})
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if d := time.Since(began); d < PipeWait || d > PipeWait+3*time.Second {
		t.Fatalf("the call took %s", d)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "still holds its output") }) {
		t.Fatalf("warnings %q", res.Warnings)
	}
}
