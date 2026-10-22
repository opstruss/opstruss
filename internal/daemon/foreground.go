package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/record"
)

// Foreground is the supervisor of a run in the process of the command
// line, while no daemon of its stack runs: the first service that starts
// makes this process the daemon of the stack, which stays in the
// foreground while its services run, as docker compose up does, and the
// runs of other command lines go to it meanwhile.
type Foreground struct {
	Dir     string
	Stack   string
	Version string
	// Ctx ends the daemon that this process becomes.
	Ctx context.Context
	// Watch gets the output and events of its services, for the terminal.
	Watch func(record.Event)

	mu     sync.Mutex
	d      *Daemon
	cancel context.CancelFunc
	served chan struct{}
}

func (f *Foreground) root() string { return filepath.Join(f.Dir, ".opstruss") }

// daemon is the daemon this process became, or nil.
func (f *Foreground) daemon() *Daemon {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.d
}

// become makes this process the daemon of the stack, once.
func (f *Foreground) become() (*Daemon, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.d != nil {
		return f.d, nil
	}
	d, err := Start(Options{Dir: f.Dir, Stack: f.Stack, Version: f.Version, Watch: f.Watch})
	var running *RunningError
	if errors.As(err, &running) {
		return nil, fmt.Errorf("the daemon of stack %s started meanwhile, pid %d; run up again, which goes to it", f.Stack, running.PID)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(f.Ctx)
	f.d, f.cancel, f.served = d, cancel, make(chan struct{})
	go func() {
		defer close(f.served)
		_ = d.Serve(ctx)
	}()
	return d, nil
}

// Start starts s in the daemon this process becomes.
func (f *Foreground) Start(ctx context.Context, s *engine.Service) error {
	d, err := f.become()
	if err != nil {
		return err
	}
	return d.sup.Start(ctx, s)
}

// Stop stops the service at addr: in the daemon this process became, or
// what a daemon that ended left of it.
func (f *Foreground) Stop(ctx context.Context, addr string) error {
	if d := f.daemon(); d != nil {
		return d.sup.Stop(ctx, addr)
	}
	_, unclear, err := Recover(f.root(), f.Stack, addr)
	var running *RunningError
	if errors.As(err, &running) {
		return fmt.Errorf("the daemon of stack %s started meanwhile, pid %d; run down again, which goes to it", f.Stack, running.PID)
	}
	if err != nil {
		return err
	}
	if len(unclear) > 0 {
		return fmt.Errorf("%s; once you are sure it is not the service's, opstruss unlock %s forgets it",
			strings.Join(unclear, "; "), strings.TrimPrefix(addr, f.Stack+"/"))
	}
	return nil
}

// Status says how the service at addr runs: in the daemon this process
// became, or in a daemon of another process, as its file records.
func (f *Foreground) Status(addr string) (engine.ServiceStatus, bool) {
	if d := f.daemon(); d != nil {
		return d.sup.Status(addr)
	}
	return status(f.root(), f.Stack, addr)
}

// Forget forgets the entries of the service at addr that cannot be proven
// its own.
func (f *Foreground) Forget(addr string) ([]string, error) {
	if d := f.daemon(); d != nil {
		return d.sup.Forget(addr)
	}
	return Forget(f.root(), f.Stack, addr)
}

// Started reports whether this process became the daemon of the stack.
func (f *Foreground) Started() bool { return f.daemon() != nil }

// Wait keeps this process the daemon of the stack while a service lives
// or a call runs, until ctx ends, then stops it with its services.
func (f *Foreground) Wait(ctx context.Context) {
	d := f.daemon()
	if d == nil {
		return
	}
	for !d.idle() && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-f.served:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	f.cancel()
	<-f.served
}
