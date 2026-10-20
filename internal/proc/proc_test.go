package proc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMain makes the test binary a program for the tests to find, because
// macOS does not show the environment of its own binaries, such as sleep.
func TestMain(m *testing.M) {
	switch os.Getenv("PROC_HELPER") {
	case "sleep":
		d, _ := time.ParseDuration(os.Getenv("PROC_HELPER_FOR"))
		time.Sleep(d)
		os.Exit(0)
	case "parent":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PROC_HELPER=sleep")
		if child.Start() == nil {
			_ = child.Wait()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// launch starts cmd in a new group, calls ready, and then reaps cmd in
// the background.
func launch(t *testing.T, cmd *exec.Cmd, ready ...func()) (Group, chan struct{}) {
	t.Helper()
	g, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Signal(syscall.SIGKILL) })
	for _, f := range ready {
		f()
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return g, done
}

// start runs script in a new group and waits until it prints a line, so
// the test signals it only after its traps are set. The line is read
// before Wait, which closes the pipe once the script exits.
func start(t *testing.T, script string) (*exec.Cmd, Group, chan struct{}) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	g, done := launch(t, cmd, func() {
		if !bufio.NewScanner(out).Scan() {
			t.Error("the script ended before it was ready")
		}
	})
	return cmd, g, done
}

func helper(kind, wait string, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), "PROC_HELPER="+kind, "PROC_HELPER_FOR="+wait), env...)
	return cmd
}

func status(cmd *exec.Cmd) syscall.WaitStatus {
	return cmd.ProcessState.Sys().(syscall.WaitStatus)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func marker() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestStopSignal(t *testing.T) {
	cmd, g, done := start(t, `trap 'exit 3' INT; echo ready; while :; do sleep 0.05; done`)
	if g.Empty() {
		t.Fatal("a running group is empty")
	}
	if err := g.Stop(syscall.SIGINT, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	<-done
	if ws := status(cmd); ws.ExitStatus() != 3 {
		t.Fatalf("exit %v, want the trap's 3", ws)
	}
}

func TestStopKillsAfterGrace(t *testing.T) {
	cmd, g, done := start(t, `trap '' INT; echo ready; sleep 30`)
	began := time.Now()
	if err := g.Stop(syscall.SIGINT, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-done
	if ws := status(cmd); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("exit %v, want SIGKILL", ws)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("stop took %s", d)
	}
}

func TestStopWhatTheLeaderLeft(t *testing.T) {
	// A background job of sh ignores SIGINT, so it outlives its leader
	// until it is killed.
	_, g, done := start(t, `sleep 30 & echo ready`)
	<-done
	if g.Empty() {
		t.Fatal("the background job is gone")
	}
	if err := g.Stop(syscall.SIGINT, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !g.Empty() {
		t.Fatal("the group is not empty after stop")
	}
}

func TestFind(t *testing.T) {
	id := marker()
	if ps, err := Find("OPSTRUSS_TEST", id); err != nil || len(ps) != 0 {
		t.Fatalf("before: %v %v", ps, err)
	}
	// The marker reaches a program that the first one starts.
	cmd := helper("parent", "30s", "OPSTRUSS_TEST="+id)
	g, done := launch(t, cmd)
	var ps []Process
	eventually(t, "the program and the one it starts", func() bool {
		ps, _ = Find("OPSTRUSS_TEST", id)
		return len(ps) == 2
	})
	if ps[0].PID != cmd.Process.Pid && ps[1].PID != cmd.Process.Pid {
		t.Fatalf("found %v, want pid %d among them", ps, cmd.Process.Pid)
	}
	if want := filepath.Base(os.Args[0]); ps[0].Command != want {
		t.Fatalf("command %q, want %q", ps[0].Command, want)
	}
	if ps, _ := Find("OPSTRUSS_TEST", id[:8]); len(ps) != 0 {
		t.Fatalf("a prefix of the value found %v", ps)
	}
	if err := g.Stop(syscall.SIGTERM, time.Second); err != nil {
		t.Fatal(err)
	}
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (Marker{Name: "OPSTRUSS_TEST", Value: id}).WaitGone(ctx, nil); err != nil {
		t.Fatalf("after stop: %v", err)
	}
}

func TestTracker(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "run.lock")
	tr, err := Track(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Track(lock); err == nil {
		t.Fatal("a second tracker took a held lock")
	}
	// sh and sleep inherit the descriptor, also as system binaries of
	// macOS, whose environment cannot be read.
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.ExtraFiles = []*os.File{tr.File()}
	g, done := launch(t, cmd)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	m := Marker{Name: "OPSTRUSS_TEST", Value: marker(), Lock: lock}
	if ps, held, err := m.Alive(); err != nil || len(ps) != 0 || !held {
		t.Fatalf("while sleep runs: %v %v %v", ps, held, err)
	}
	if err := g.Stop(syscall.SIGTERM, time.Second); err != nil {
		t.Fatal(err)
	}
	<-done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.WaitGone(ctx, nil); err != nil {
		t.Fatalf("after stop: %v", err)
	}
}

func TestWaitGone(t *testing.T) {
	id := marker()
	cmd := helper("sleep", "1s", "OPSTRUSS_TEST="+id)
	_, done := launch(t, cmd)
	eventually(t, "the program", func() bool {
		ps, _ := Find("OPSTRUSS_TEST", id)
		return len(ps) == 1
	})
	var reported []Process
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := Marker{Name: "OPSTRUSS_TEST", Value: id}
	if err := m.WaitGone(ctx, func(ps []Process, _ bool) { reported = ps }); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(reported) != 1 || reported[0].PID != cmd.Process.Pid {
		t.Fatalf("reported %v, want pid %d", reported, cmd.Process.Pid)
	}

	id = marker()
	launch(t, helper("sleep", "30s", "OPSTRUSS_TEST="+id))
	ctx, cancel = context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	m = Marker{Name: "OPSTRUSS_TEST", Value: id}
	eventually(t, "the program", func() bool {
		ps, _ := Find("OPSTRUSS_TEST", id)
		return len(ps) == 1
	})
	if err := m.WaitGone(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a program that keeps running: %v", err)
	}
}

func TestParseSignal(t *testing.T) {
	if s, err := ParseSignal("SIGTERM"); err != nil || s != syscall.SIGTERM {
		t.Fatalf("SIGTERM: %v %v", s, err)
	}
	if _, err := ParseSignal("SIGFOO"); err == nil {
		t.Fatal("SIGFOO: want an error")
	}
}
