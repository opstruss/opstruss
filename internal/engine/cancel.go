package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
)

// errRequested is the cause of a run that a request in its cancel.jsonl
// stopped.
var errRequested = errors.New("cancelled by request")

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

// cancelledBy is the reason that the record of a run gives when a request
// stopped it, or empty.
func (e *engine) cancelledBy() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancelReq == nil || !errors.Is(context.Cause(e.ctx), errRequested) {
		return ""
	}
	return e.cancelReq.Reason
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
	return model.Load(opts.Dir, opts.Stack)
}

// runOf reads the record of the run id of the stack of opts, without the
// inputs that a run of the project needs, and the directory it is in.
func runOf(opts Options, id string) (*record.Record, string, error) {
	p, err := project(opts)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(p.Dir, ".opstruss", "runs", p.StackName, filepath.Base(id))
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
