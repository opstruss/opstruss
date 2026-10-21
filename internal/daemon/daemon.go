// Package daemon is the OpsTruss that stays for a stack, which up -d
// starts: one per stack, held by a lock, with a socket on which it runs the
// calls of the engine API, so the runs of the command line share its
// process, its locks, and its concurrency.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
)

// Version is the version of <stack>.json.
const Version = 1

// File is daemon/<stack>.json: how to reach the daemon and how to stop it.
// The daemon lives while it holds daemon/<stack>.lock, so the file outlives
// it.
type File struct {
	Version  int    `json:"version"`
	PID      int    `json:"pid"`
	Start    string `json:"start"` // when the process started, from proc.StartTime
	Started  string `json:"started"`
	OpsTruss string `json:"opstruss"`
	Socket   string `json:"socket"`
}

// maxSocket is the longest path of a Unix socket: 104 bytes with its NUL
// on macOS, 108 on Linux.
const maxSocket = 103

func lockPath(root, stack string) string   { return filepath.Join(root, "daemon", stack+".lock") }
func filePath(root, stack string) string   { return filepath.Join(root, "daemon", stack+".json") }
func socketPath(root, stack string) string { return filepath.Join(root, "daemon", stack+".sock") }

// LogPath is the log of the daemon of stack, under root, the project's
// .opstruss.
func LogPath(root, stack string) string { return filepath.Join(root, "logs", stack, "_daemon.jsonl") }

// Options says which daemon to start.
type Options struct {
	Dir     string // the project
	Stack   string
	Version string // of OpsTruss
}

// RunningError is a daemon of the stack that runs already.
type RunningError struct {
	Stack string
	PID   int
}

func (e *RunningError) Error() string {
	return fmt.Sprintf("the daemon of stack %s runs already, pid %d", e.Stack, e.PID)
}

// Daemon is a daemon that holds its lock and listens on its socket.
type Daemon struct {
	opts Options
	root string
	lock *os.File
	ln   net.Listener
	tmp  string // the directory of the socket under $TMPDIR, when it is there
	log  *record.Log

	mu       sync.Mutex
	stopping bool
	calls    sync.WaitGroup
}

// Start takes the lock of the stack's daemon, listens on its socket, and
// writes <stack>.json.
func Start(opts Options) (_ *Daemon, err error) {
	root := filepath.Join(opts.Dir, ".opstruss")
	if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o700); err != nil {
		return nil, err
	}
	d := &Daemon{opts: opts, root: root}
	defer func() {
		if err != nil {
			d.close()
		}
	}()
	if d.lock, err = os.OpenFile(lockPath(root, opts.Stack), os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			running := &RunningError{Stack: opts.Stack}
			if f, err := Read(root, opts.Stack); err == nil {
				running.PID = f.PID
			}
			return nil, running
		}
		return nil, err
	}
	// The file of a newer OpsTruss may hold what this one would drop.
	if _, err := Read(root, opts.Stack); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sock := socketPath(root, opts.Stack)
	if len(sock) > maxSocket {
		if d.tmp, err = socketDir(); err != nil {
			return nil, err
		}
		sock = filepath.Join(d.tmp, opts.Stack+".sock")
	} else if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if d.ln, err = net.Listen("unix", sock); err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		return nil, err
	}
	if d.log, err = record.OpenLog(LogPath(root, opts.Stack)); err != nil {
		return nil, err
	}
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		return nil, err
	}
	f := File{Version: Version, PID: os.Getpid(), Start: start, Started: time.Now().UTC().Format(time.RFC3339),
		OpsTruss: opts.Version, Socket: sock}
	if err := writeFile(filePath(root, opts.Stack), f); err != nil {
		return nil, err
	}
	d.say("info", "daemon started: pid %d, opstruss %s", f.PID, f.OpsTruss)
	return d, nil
}

// socketDir makes a directory for the socket under $TMPDIR, or under /tmp
// when $TMPDIR is too long itself.
func socketDir() (string, error) {
	base := os.TempDir()
	if len(base) > maxSocket-48 {
		base = "/tmp"
	}
	return os.MkdirTemp(base, "opstruss-")
}

// Serve runs the calls that come to the socket until ctx ends, or until
// the lock file is gone. Then it takes no new call, cancels the calls that
// run, waits for them, and closes.
func (d *Daemon) Serve(ctx context.Context) error {
	calls, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &api.Server{Version: d.opts.Version, Stopping: d.isStopping,
		Call: func(ctx context.Context, method string, r api.Request, h api.Handlers) (*api.Result, error) {
			return api.New(h).Call(ctx, method, r)
		}}
	go d.accept(calls, srv)
	select {
	case <-ctx.Done():
	case <-d.lockLost(ctx):
		d.say("warn", "%s is gone, so another daemon could start; this one stops", lockPath(d.root, d.opts.Stack))
	}
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
	cancel()
	d.calls.Wait()
	d.say("info", "daemon stopped")
	d.close()
	return nil
}

func (d *Daemon) isStopping() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopping
}

// accept serves each connection in its own goroutine; those that come
// while the daemon stops only hear that it does.
func (d *Daemon) accept(ctx context.Context, srv *api.Server) {
	for {
		nc, err := d.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// Out of descriptors, for example: try again shortly.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		d.mu.Lock()
		if d.stopping {
			d.mu.Unlock()
			go srv.ServeConn(ctx, nc)
			continue
		}
		d.calls.Add(1)
		d.mu.Unlock()
		go func() {
			defer d.calls.Done()
			srv.ServeConn(ctx, nc)
		}()
	}
}

// lockLost is closed when the lock file is gone or replaced, as when
// .opstruss is deleted: a second daemon could then take a new lock.
func (d *Daemon) lockLost(ctx context.Context) <-chan struct{} {
	lost := make(chan struct{})
	held, err := d.lock.Stat()
	if err != nil {
		close(lost)
		return lost
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if now, err := os.Stat(lockPath(d.root, d.opts.Stack)); err != nil || !os.SameFile(held, now) {
					close(lost)
					return
				}
			}
		}
	}()
	return lost
}

// close lets go of the socket, its directory, the log, and the lock.
func (d *Daemon) close() {
	if d.ln != nil {
		_ = d.ln.Close()
	}
	if d.tmp != "" {
		_ = os.RemoveAll(d.tmp)
	}
	if d.log != nil {
		_ = d.log.Close()
	}
	if d.lock != nil {
		_ = d.lock.Close()
	}
}

// say writes an event of the daemon to its log.
func (d *Daemon) say(level, format string, args ...any) {
	d.log.Emit(record.Event{Type: "daemon", Level: level, Message: fmt.Sprintf(format, args...)})
}

// Read reads <stack>.json under root; one of a newer version is refused,
// as other data of a newer OpsTruss is.
func Read(root, stack string) (*File, error) {
	p := filePath(root, stack)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if f.Version > Version {
		return nil, fmt.Errorf("%s has version %d, from a newer opstruss; this one reads up to %d", p, f.Version, Version)
	}
	return &f, nil
}

// Alive reports whether a daemon of stack holds its lock under root.
func Alive(root, stack string) bool {
	f, err := os.OpenFile(lockPath(root, stack), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

// Dial connects to the daemon of stack under root, or returns nil when
// none lives. A daemon that holds its lock without listening yet is
// starting, and is waited for a moment.
func Dial(root, stack, version string, terminal bool) (*api.Client, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !Alive(root, stack) {
			return nil, nil
		}
		f, err := Read(root, stack)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			c, derr := api.Dial(f.Socket, version, terminal)
			var pe *api.ProtocolError
			if derr == nil || errors.As(derr, &pe) {
				return c, derr
			}
			err = derr
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the daemon of stack %s does not answer: %w", stack, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stop stops the daemon of stack with SIGTERM, once its pid and start time
// match <stack>.json, so that a pid used again by another process is not
// hit, and waits until the daemon lets go of its lock. It returns the pid,
// or 0 when no daemon runs.
func Stop(root, stack string) (int, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !Alive(root, stack) {
			return 0, nil
		}
		f, err := Read(root, stack)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		if err == nil {
			if start, err := proc.StartTime(f.PID); err == nil && start == f.Start {
				if err := syscall.Kill(f.PID, syscall.SIGTERM); err != nil {
					return 0, err
				}
				return f.PID, wait(root, stack)
			}
		}
		// A daemon that holds its lock without a file that names it is
		// starting.
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("a daemon of stack %s holds %s, but %s does not name it", stack,
				lockPath(root, stack), filePath(root, stack))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// wait waits until the daemon of stack lets go of its lock.
func wait(root, stack string) error {
	f, err := os.OpenFile(lockPath(root, stack), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// writeFile replaces p with v as JSON, atomically.
func writeFile(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
