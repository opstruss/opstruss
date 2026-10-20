package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// PollInterval is how often WaitGone looks for the processes again.
const PollInterval = 500 * time.Millisecond

// Process is a process found by a marker in its environment.
type Process struct {
	PID     int
	Command string
}

func (p Process) String() string { return fmt.Sprintf("pid %d (%s)", p.PID, p.Command) }

// Find returns the processes of this user, other than this one, whose
// environment holds name=value, such as OPSTRUSS_RUN=<run>. It reads the
// environment each process started with, which a program passes on to
// every program it starts, as the runner of GitHub Actions finds orphan
// processes by RUNNER_TRACKING_ID. A program that clears its environment
// or writes over it is not found, and neither is a system binary of macOS,
// whose environment macOS does not show.
func Find(name, value string) ([]Process, error) {
	return find([]byte(name + "=" + value))
}

// Tracker holds the lock that marks the programs of one run: every program
// inherits its descriptor, and flock keeps the lock held while any process
// has it, also after OpsTruss dies, as flock(1) does for the programs it
// starts.
type Tracker struct{ f *os.File }

// Track takes the lock at path.
func Track(path string) (*Tracker, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Tracker{f: f}, nil
}

// File is the descriptor that a program inherits, as its fd 3.
func (t *Tracker) File() *os.File { return t.f }

// Close lets go of this process's hold; programs that still have the
// descriptor keep the lock.
func (t *Tracker) Close() error { return t.f.Close() }

// Marker finds the programs of one run in two ways, because a program can
// lose either: the environment entry Name=Value, and the lock at Lock that
// each program inherits from a Tracker.
type Marker struct {
	Name, Value string
	Lock        string
}

// Alive returns the processes found by the environment entry, and whether
// some process still holds the lock.
func (m Marker) Alive() ([]Process, bool, error) {
	ps, err := Find(m.Name, m.Value)
	if err != nil {
		return nil, false, err
	}
	held, err := heldLock(m.Lock)
	return ps, held, err
}

// WaitGone waits while m finds a live program, and calls report, when it
// is not nil, each time what it finds changes, until nothing is left or
// ctx ends.
func (m Marker) WaitGone(ctx context.Context, report func(ps []Process, held bool)) error {
	var last string
	for {
		ps, held, err := m.Alive()
		if err != nil {
			return err
		}
		if len(ps) == 0 && !held {
			return nil
		}
		if seen := fmt.Sprint(ps, held); seen != last && report != nil {
			report(ps, held)
			last = seen
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(PollInterval):
		}
	}
}

// heldLock reports whether a process holds the lock at path; a lock file
// that is gone has no holder.
func heldLock(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// holds reports whether an environment block of NUL-terminated entries has
// entry.
func holds(env, entry []byte) bool {
	for len(env) > 0 {
		var e []byte
		e, env, _ = bytes.Cut(env, []byte{0})
		if bytes.Equal(e, entry) {
			return true
		}
	}
	return false
}
