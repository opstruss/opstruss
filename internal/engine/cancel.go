package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/sources"
	"golang.org/x/sys/unix"
)

// errRequested is the cause of a run that a request in its cancel.jsonl
// stopped.
var errRequested = errors.New("cancelled by request")

// Interrupted is the cause of a run that a signal stopped, as Ctrl-C at
// the terminal of the caller does, or SIGTERM at the daemon it runs in:
// the run ends cancelled with the exit code that a shell reports, 128 and
// the signal.
type Interrupted struct{ Signal syscall.Signal }

func (i Interrupted) Error() string { return "interrupted by " + unix.SignalName(i.Signal) }

// control makes the context of a run that stops on the requests of its
// cancel.jsonl: the first cancels it, as Ctrl-C at its terminal does, and
// one with force, first or later, kills its programs at once.
func (e *engine) control() {
	ctx, cancel := context.WithCancelCause(e.ctx)
	ctx, kill := proc.WithKill(ctx)
	e.ctx = ctx
	e.release = func() { kill(); cancel(nil) }
	e.rec.WatchCancel(func(c record.Cancel) {
		e.mu.Lock()
		first, killed := e.cancelReq == nil, e.killReq
		if first {
			e.cancelReq = &c
		}
		e.killReq = e.killReq || c.Force
		e.mu.Unlock()
		if first {
			e.warn("", "cancel requested by %s; the programs stop within their stop.grace", requester(c))
			cancel(errRequested)
		}
		if c.Force && !killed {
			e.warn("", "cancel with force requested by %s; the programs are killed at once", requester(c))
			kill()
		}
	})
}

// requester says who asked for c, as its events keep it.
func requester(c record.Cancel) string {
	s := fmt.Sprintf("%s@%s (pid %d", c.User, c.Hostname, c.PID)
	if c.Run != "" {
		s += ", run " + c.Run
	}
	switch c.Reason {
	case "concurrency":
		s += ", concurrency: cancel"
	default:
		s += ", opstruss runs cancel"
	}
	return s + ")"
}

// cancelledBy is the reason that the record of a run gives when a signal
// or a request stopped it, or empty, with the exit code of such a run.
func (e *engine) cancelledBy() (string, int) {
	cause := context.Cause(e.ctx)
	var in Interrupted
	if errors.As(cause, &in) {
		return "signal", 128 + int(in.Signal)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancelReq == nil || !errors.Is(cause, errRequested) {
		return "", 0
	}
	return e.cancelReq.Reason, 1
}

// stopped reports whether a unit or task of a run ended cancelled.
func stopped(units map[string]*record.Unit, tasks map[string]*record.Task) bool {
	for _, u := range units {
		if u.Outcome == cancelled {
			return true
		}
	}
	for _, t := range tasks {
		if t.Outcome == cancelled {
			return true
		}
	}
	return false
}

// requestCancel asks the run in dir to cancel, for reason, as this
// process, which caller runs when it is a run: whoever runs it, here or in
// another process.
func requestCancel(dir, reason string, force bool, caller string) error {
	return record.RequestCancel(dir, record.Cancel{
		Reason: reason, Force: force, User: username(), Hostname: hostname(), PID: os.Getpid(), Run: caller})
}

// project is the project of opts as its files say, without the inputs
// that a run of it needs, for what only reads or asks the runs.
func project(opts Options) (*model.Project, error) {
	if opts.Model != nil {
		return opts.Model, nil
	}
	return model.LoadWith(context.Background(), opts.Dir, opts.Stack, sources.Options{Locked: opts.Locked})
}

// runOf reads the record of the run id of the stack of opts, without the
// inputs that a run of the project needs, and the directory it is in.
func runOf(opts Options, id string) (*record.Record, string, error) {
	p, err := project(opts)
	if err != nil {
		return nil, "", err
	}
	if opts.Model == nil {
		defer p.Close()
	}
	dir, err := runDir(p, id)
	if err != nil {
		return nil, "", err
	}
	rec, err := record.Read(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("stack %s has no run %s", p.StackName, id)
	}
	return rec, dir, err
}

// Cancel asks the run id of the stack to cancel, or with force to kill its
// programs at once, and waits until it ends or ctx does. It returns the
// record as the run left it, or the one before the request when the run
// kept none, as a run that only read; ErrEnded says the run had ended.
func Cancel(ctx context.Context, opts Options, id string, force bool) (*record.Record, error) {
	rec, dir, err := runOf(opts, id)
	if err != nil {
		return nil, err
	}
	caller := ""
	if opts.LookupEnv != nil {
		caller, _ = opts.LookupEnv("OPSTRUSS_RUN")
	}
	if err := requestCancel(dir, "requested", force, caller); err != nil {
		if errors.Is(err, record.ErrEnded) {
			return rec, err
		}
		return nil, err
	}
	if err := record.WaitEnded(ctx, dir); err != nil {
		return nil, err
	}
	if after, err := record.Read(dir); err == nil {
		rec = after
	}
	return rec, nil
}
