// Package proc runs each program as the leader of its own process group,
// stops a group with a signal, a grace period, and a kill, and finds the
// processes of this user that carry a marker in their environment.
package proc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// KillWait is how long Stop waits for a group to empty after SIGKILL, for
// processes that the kernel has not removed yet.
const KillWait = 5 * time.Second

type killKey struct{}

// WithKill returns a copy of parent that carries a kill channel, which
// kill closes, and which closes too when the one of parent does. A group
// that a cancel of ctx stops is killed once it closes, without the rest
// of its grace, as a second Ctrl-C asks. Callers defer kill as they defer
// a cancel, which also ends the wait for the channel of parent.
func WithKill(parent context.Context) (ctx context.Context, kill func()) {
	ch := make(chan struct{})
	var once sync.Once
	kill = func() { once.Do(func() { close(ch) }) }
	if up := Kill(parent); up != nil {
		go func() {
			select {
			case <-up:
				kill()
			case <-ch:
			}
		}()
	}
	return context.WithValue(parent, killKey{}, (<-chan struct{})(ch)), kill
}

// Kill is the kill channel that ctx carries, or nil, which never closes.
func Kill(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(killKey{}).(<-chan struct{})
	return ch
}

// Start starts cmd as the leader of a new process group, so a stop reaches
// every program it starts, such as tofu under sh, and Ctrl-C at the
// terminal reaches OpsTruss alone, which then stops each group itself.
func Start(cmd *exec.Cmd) (Group, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return Group(cmd.Process.Pid), nil
}

// Wait waits for the program that cmd started as the leader of g, as Start
// starts it, and then stops what it left in g, so nothing of it runs once
// Wait returns. When ctx ends first, the group is stopped with sig, and
// killed after grace or once the kill channel of ctx closes. Stopped says
// that ctx ended the program; warnings say what else had to be stopped.
func Wait(ctx context.Context, cmd *exec.Cmd, g Group, sig syscall.Signal, grace time.Duration) (stopped bool, warnings []string) {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	// A second Ctrl-C kills at once what the first one stops.
	kill := Kill(ctx)
	select {
	case <-exited:
	case <-ctx.Done():
		stopped = true
		if err := g.Stop(sig, grace, kill); err != nil {
			warnings = append(warnings, err.Error())
		}
		<-exited
	}
	if !g.Empty() {
		warnings = append(warnings, "stopped the processes that the program left in its process group")
		if err := g.Stop(sig, grace, kill); err != nil {
			warnings = append(warnings, err.Error())
		}
	}
	return stopped, warnings
}

// ExitCode is the exit code of a program that ended with state: 128 + the
// signal for one that a signal ended, as in a shell.
func ExitCode(state *os.ProcessState) int {
	ws := state.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// Group is a process group, numbered by the pid of its leader. The number
// is not reused while the group has members.
type Group int

// Signal sends sig to every process in the group, then SIGCONT, so a
// stopped process, such as one that read the terminal from the background,
// gets it too, as a shell does for a stopped job. A group that Empty finds
// empty takes no signal and is no error: on macOS, one whose processes
// just exited and wait to be reaped answers EPERM.
func (g Group) Signal(sig syscall.Signal) error {
	err := syscall.Kill(-int(g), sig)
	if err == nil && sig != syscall.SIGKILL && sig != syscall.SIGCONT {
		err = syscall.Kill(-int(g), syscall.SIGCONT)
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

// Empty reports whether no process of this user is left in the group. A
// leader that exited counts until it is reaped. A group whose members all
// belong to another user (EPERM) holds nothing of ours: its number was
// reused, or a setuid program is in it, which no signal of ours reaches.
func (g Group) Empty() bool {
	err := syscall.Kill(-int(g), 0)
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}

// Stop sends sig to the group, waits up to grace for it to empty, or
// until kill closes, and then kills what is left. Whoever started the
// leader reaps it meanwhile, as cmd.Wait does, or the group never empties.
func (g Group) Stop(sig syscall.Signal, grace time.Duration, kill <-chan struct{}) error {
	if g.Empty() {
		return nil
	}
	if err := g.Signal(sig); err != nil {
		return err
	}
	if g.wait(grace, kill) {
		return nil
	}
	if err := g.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	if g.wait(KillWait, nil) {
		return nil
	}
	return fmt.Errorf("process group %d still has processes %s after SIGKILL", g, KillWait)
}

// GroupOf returns the process group of pid.
func GroupOf(pid int) (Group, error) {
	g, err := unix.Getpgid(pid)
	return Group(g), err
}

// Session returns the session of pid, numbered by the pid of its leader.
// A process never moves to another session than the one it starts in or
// makes, so a process group never does either.
func Session(pid int) (int, error) { return unix.Getsid(pid) }

// wait polls until the group is empty, d has passed, or kill closes.
func (g Group) wait(d time.Duration, kill <-chan struct{}) bool {
	deadline := time.Now().Add(d)
	for delay := 5 * time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		if g.Empty() {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		select {
		case <-kill:
			return false
		case <-time.After(min(delay, left)):
		}
	}
}

// signals are the signals a program can be stopped with.
var signals = map[string]syscall.Signal{
	"SIGHUP":   syscall.SIGHUP,
	"SIGINT":   syscall.SIGINT,
	"SIGQUIT":  syscall.SIGQUIT,
	"SIGABRT":  syscall.SIGABRT,
	"SIGKILL":  syscall.SIGKILL,
	"SIGUSR1":  syscall.SIGUSR1,
	"SIGUSR2":  syscall.SIGUSR2,
	"SIGALRM":  syscall.SIGALRM,
	"SIGTERM":  syscall.SIGTERM,
	"SIGWINCH": syscall.SIGWINCH,
}

// SignalName is the name of sig, such as SIGTERM, as ParseSignal reads it.
func SignalName(sig syscall.Signal) string {
	for name, s := range signals {
		if s == sig {
			return name
		}
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// ParseSignal returns the signal named name, such as SIGTERM.
func ParseSignal(name string) (syscall.Signal, error) {
	if s, ok := signals[name]; ok {
		return s, nil
	}
	return 0, fmt.Errorf("unknown signal %s; use one of %s", name, strings.Join(slices.Sorted(maps.Keys(signals)), ", "))
}
