package proc

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSudo is a sudo that runs its command as this user, as sudo does
// where it execs the command itself: FAKE_REFUSE makes it refuse, and
// FAKE_LOG_INPUT puts a pipe on the command's stdin, as sudoers log_input
// does.
const fakeSudo = `#!/bin/sh
if [ -n "$FAKE_REFUSE" ]; then echo 'sudo: a password is required' >&2; exit 1; fi
while [ $# -gt 0 ]; do case $1 in -n) shift ;; --) shift; break ;; *) break ;; esac; done
export SUDO_UID=$(id -u) SUDO_GID=$(id -g) SUDO_USER=$(id -un)
if [ -n "$FAKE_LOG_INPUT" ]; then
  d=$(mktemp -d) && mkfifo "$d/in" || exit 1
  cat >"$d/in" & c=$!
  "$@" <"$d/in"; r=$?
  kill $c 2>/dev/null; rm -rf "$d"; exit $r
fi
exec "$@"
`

// sudoFor is a Sudo with fakeSudo and this test binary as opstruss.
func sudoFor(t *testing.T, env ...string) Sudo {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(fakeSudo), 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Sudo{Path: path, Exe: exe, Env: append([]string{"PATH=" + os.Getenv("PATH")}, env...)}
}

// rootRun starts script through s with stdout and stderr as one pipe, and
// a pipe as descriptor 3, and returns what both pipes got and how it ended.
func rootRun(t *testing.T, ctx context.Context, s Sudo, script string, ready func(*SudoProgram)) (out, three string, code int, stopped bool, warnings []string) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	or, ow, _ := os.Pipe()
	tr, tw, _ := os.Pipe()
	p, err := s.Start(ctx, Spec{Path: "/bin/sh", Args: []string{"sh", "-c", script}, Dir: t.TempDir(), Env: []string{"FOO=bar", "PATH=/usr/bin:/bin"},
		Signal: "SIGINT", Grace: 5 * time.Second}, []*os.File{null, ow, ow, tw})
	_ = ow.Close()
	_ = tw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if ready != nil {
		ready(p)
	}
	read := func(r *os.File) chan string {
		got := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			got <- string(b)
		}()
		return got
	}
	gotOut, gotThree := read(or), read(tr)
	code, stopped, warnings, err = p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return <-gotOut, <-gotThree, code, stopped, warnings
}

// TestSudoRun: the program that __sudo starts gets the descriptors, the
// environment, and the facts of sudo, and its exit code comes back, also
// 128 + the signal that ended it.
func TestSudoRun(t *testing.T) {
	s := sudoFor(t)
	out, three, code, stopped, _ := rootRun(t, t.Context(), s, `echo "out $FOO"; echo err >&2; echo three >&3; test -n "$SUDO_UID" || exit 9; exit 3`, nil)
	if out != "out bar\nerr\n" || three != "three\n" || code != 3 || stopped {
		t.Fatalf("out %q, fd 3 %q, exit %d, stopped %v", out, three, code, stopped)
	}
	if _, _, code, _, _ = rootRun(t, t.Context(), s, `kill -TERM $$`, nil); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %d, want %d", code, 128+int(syscall.SIGTERM))
	}
}

// TestSudoStop: a cancel stops the program with its signal, and a program
// that its signal does not end is killed by the kill channel.
func TestSudoStop(t *testing.T) {
	s := sudoFor(t)
	ctx, cancel := context.WithCancel(t.Context())
	out, _, code, stopped, _ := rootRun(t, ctx, s, `trap 'echo stopped; exit 7' INT; echo up; while :; do sleep 0.05; done`, func(*SudoProgram) {
		time.AfterFunc(300*time.Millisecond, cancel)
	})
	if out != "up\nstopped\n" || code != 7 || !stopped {
		t.Fatalf("out %q, exit %d, stopped %v", out, code, stopped)
	}

	ctx, cancel = context.WithCancel(t.Context())
	ctx, kill := WithKill(ctx)
	defer kill()
	begun := time.Now()
	_, _, code, _, _ = rootRun(t, ctx, s, `trap '' INT; while :; do sleep 0.05; done`, func(*SudoProgram) {
		time.AfterFunc(200*time.Millisecond, cancel)
		time.AfterFunc(400*time.Millisecond, kill)
	})
	if code != 128+int(syscall.SIGKILL) || time.Since(begun) > 4*time.Second {
		t.Fatalf("exit %d after %s, want SIGKILL before the grace", code, time.Since(begun))
	}
}

// TestSudoGone: when the socket closes, as when OpsTruss dies, __sudo
// stops the program.
func TestSudoGone(t *testing.T) {
	s := sudoFor(t)
	mark := filepath.Join(t.TempDir(), "mark")
	null, _ := os.Open(os.DevNull)
	defer func() { _ = null.Close() }()
	p, err := s.Start(t.Context(), Spec{Path: "/bin/sh", Args: []string{"sh", "-c", `trap 'echo gone > "$MARK"; exit 0' INT; while :; do sleep 0.05; done`},
		Dir: t.TempDir(), Env: []string{"MARK=" + mark, "PATH=/usr/bin:/bin"}, Signal: "SIGINT", Grace: 5 * time.Second}, []*os.File{null, null, null})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	p.ended()
	if b, _ := os.ReadFile(mark); string(b) != "gone\n" {
		t.Fatalf("the program did not stop when the socket closed: mark %q", b)
	}
}

// TestSudoRefused: a sudo that does not start __sudo gives what it said,
// and __sudo without its socket, as log_input gives it, says why.
func TestSudoRefused(t *testing.T) {
	null, _ := os.Open(os.DevNull)
	defer func() { _ = null.Close() }()
	spec := Spec{Path: "/bin/sh", Args: []string{"sh", "-c", "exit 0"}, Dir: t.TempDir(), Signal: "SIGINT"}
	_, err := sudoFor(t, "FAKE_REFUSE=1").Start(t.Context(), spec, []*os.File{null, null, null})
	var refused *SudoRefused
	if !errors.As(err, &refused) || refused.Message != "sudo: a password is required" {
		t.Fatalf("got %v, want SudoRefused with what sudo said", err)
	}
	_, err = sudoFor(t, "FAKE_LOG_INPUT=1").Start(t.Context(), spec, []*os.File{null, null, null})
	if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), "!log_input") {
		t.Fatalf("got %v, want an error about log_input", err)
	}
	spec.Path = "/no/such/program"
	_, err = sudoFor(t).Start(t.Context(), spec, []*os.File{null, null, null})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want a program that does not exist", err)
	}
}
