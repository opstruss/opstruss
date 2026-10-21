package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
)

// taskHolder is who holds the concurrency lock of a task, in its lock file.
type taskHolder struct {
	Run  string `json:"run"`
	PID  int    `json:"pid"`
	Task string `json:"task"` // the root task of the holder's run
}

var taskMutexes sync.Map // lock path → *sync.Mutex, for calls in one process

// taskHolders keeps the cancel of each call that holds a task lock in this
// process, so a call with concurrency: cancel stops it from any run of the
// process, as the runs of a daemon are.
var taskHolders sync.Map // lock path → context.CancelCauseFunc

// taskLock takes the concurrency lock of a call of a task in this stack
// and checkout, .opstruss/tasks/<stack>/<task>.lock, or
// <task>[<group>].lock with a group, as flock(1) in cron: queue waits for
// it, skip does not, cancel stops the holder first, and parallel takes
// none. The context it returns ends when another call cancels this one.
func (e *engine) taskLock(ctx context.Context, name string, t *model.Task, s *expr.Scope) (context.Context, func(), bool, error) {
	mode, group := "queue", ""
	if c := t.Concurrency; c != nil {
		mode = cmp.Or(c.Mode, "queue")
		if c.Group != "" {
			g, sensitive, err := expr.EvalTextMarked(c.Group, s)
			if err != nil {
				return nil, nil, false, fmt.Errorf("concurrency.group: %w", err)
			}
			if sensitive || e.redact.Contains(g) {
				return nil, nil, false, errors.New("concurrency.group: a sensitive value is never a group, which names a lock file")
			}
			if !forEachKey.MatchString(g) {
				return nil, nil, false, fmt.Errorf("concurrency.group %q has more than lower-case letters, digits, '.', '-', and '_'", g)
			}
			group = g
		}
	}
	if mode == "parallel" {
		return ctx, func() {}, false, nil
	}
	file := name
	if group != "" {
		file += "[" + group + "]"
	}
	p := filepath.Join(e.root, "tasks", e.p.StackName, file+".lock")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, nil, false, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, false, err
	}
	m, _ := taskMutexes.LoadOrStore(p, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	cancelled := false
	waiting := false
	for delay := 20 * time.Millisecond; ; delay = min(2*delay, 500*time.Millisecond) {
		if mu.TryLock() {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				return e.holdTask(ctx, p, f, mu)
			}
			mu.Unlock()
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				_ = f.Close()
				return nil, nil, false, err
			}
		}
		switch {
		case mode == "skip":
			_ = f.Close()
			return nil, nil, true, nil
		case mode == "cancel" && !cancelled:
			cancelled = true
			e.cancelHolder(p, f, name)
		}
		if !waiting {
			waiting = true
			e.sayTask(name, "", "waiting for the run of %s that holds its concurrency lock", file)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, nil, false, context.Cause(ctx)
		case <-time.After(delay):
		}
	}
}

// holdTask writes this run into the lock file and keeps the cancel of the
// call, so a call with concurrency: cancel in this process can stop it.
func (e *engine) holdTask(ctx context.Context, p string, f *os.File, mu *sync.Mutex) (context.Context, func(), bool, error) {
	data, _ := json.Marshal(taskHolder{Run: e.runID, PID: os.Getpid(), Task: e.rootTask})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(data, 0)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	taskHolders.Store(p, cancel)
	release := func() {
		taskHolders.Delete(p)
		cancel(nil)
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		mu.Unlock()
	}
	return ctx, release, false, nil
}

// cancelHolder stops the call that holds the lock at p: in this process
// its context; in another, the whole run with SIGINT, but only when the
// root task of that run is this task, so a deploy that calls notify does
// not stop with it.
func (e *engine) cancelHolder(p string, f *os.File, name string) {
	if cancel, ok := taskHolders.Load(p); ok {
		cancel.(context.CancelCauseFunc)(errors.New("cancelled by a call with concurrency: cancel"))
		return
	}
	var h taskHolder
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, 0)
	if json.Unmarshal(buf[:n], &h) != nil || h.PID <= 0 {
		return
	}
	if h.Task != name {
		e.warn("", "concurrency: cancel waits for run %s, whose root task is %s, not %s", h.Run, h.Task, name)
		return
	}
	// A pid is reused once its process is gone; the holder is an OpsTruss,
	// and never this one, whose calls are in taskHolders.
	if c, err := proc.Command(h.PID); err != nil || c != filepath.Base(os.Args[0]) || h.PID == os.Getpid() {
		return
	}
	_ = syscall.Kill(h.PID, syscall.SIGINT)
}
