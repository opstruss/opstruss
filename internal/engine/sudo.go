package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
)

// sudoKeep is how often a run keeps the ticket of sudo while it lasts, as
// the keep-alive loops of yay and of dotfiles for macOS do.
var sudoKeep = time.Minute

// sudoTries is how often a run asks for the password of sudo, as sudo does.
const sudoTries = 3

// sudoSession is what the programs of one run share of sudo: one question
// for the password at a time, and the ticket that sudo keeps once it took
// one, which the run keeps alive until it ends.
type sudoSession struct {
	mu   sync.Mutex
	took int                // the passwords that sudo took, so a program that waited for a question asks none
	stop context.CancelFunc // of the keep-alive, once it runs
}

// execCall runs c, as root through sudo when c says so: sudo -n starts it, and
// when sudo wants a password, the terminal of the run gives it once to
// sudo -v, and c starts again. ev says whose program it is.
func (e *engine) execCall(ctx context.Context, c *plugin.Call, ev Event) (*plugin.Result, error) {
	if !c.Sudo {
		return e.run.Exec(ctx, c)
	}
	ran := Event{Type: "diagnostic", Level: "debug", Address: ev.Address, Task: ev.Task, Step: ev.Step,
		Message: "runs as root through sudo", Sudo: true}
	if os.Geteuid() == 0 {
		// OpsTruss is root already, as in a container: no sudo, no password.
		ran.Message = "runs as root, as OpsTruss does"
		e.emit(ran)
		return e.run.Exec(ctx, c)
	}
	e.emit(ran)
	// The first program may run long, as an install does, so the ticket is
	// kept from its start.
	e.keepSudo()
	s := &e.sudo
	s.mu.Lock()
	took := s.took
	s.mu.Unlock()
	res, err := e.run.Exec(ctx, c)
	var refused *proc.SudoRefused
	if errors.As(err, &refused) {
		if err = e.sudoPassword(ctx, took, refused, ev); err != nil {
			return nil, err
		}
		if res, err = e.run.Exec(ctx, c); errors.As(err, &refused) {
			return nil, fmt.Errorf("sudo took the password and still refused to run %s: %s", e.sudoCommand(), refused.Message)
		}
	}
	return res, err
}

// sudoPassword asks at the terminal for the password of sudo, which sudo
// -v reads once, unless sudo took one since took; a run without a terminal
// fails, with what sudo said.
func (e *engine) sudoPassword(ctx context.Context, took int, refused *proc.SudoRefused, ev Event) error {
	s := &e.sudo
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.took != took {
		return nil
	}
	if e.opts.Ask == nil {
		return fmt.Errorf("sudo needs a password, and this run has no terminal; run sudo -v in this terminal first, or allow %s in sudoers without a password (%s)",
			e.sudoCommand(), refused.Message)
	}
	for try := 1; ; try++ {
		password, err := e.opts.Ask(ctx, "sudo password for "+username(), &model.Input{Sensitive: true})
		if err != nil {
			return fmt.Errorf("the password of sudo: %w", err)
		}
		e.redact.Add(password)
		if err = e.run.SudoValidate(ctx, password); err == nil {
			s.took++
			return nil
		}
		if !errors.As(err, &refused) || try == sudoTries {
			return fmt.Errorf("sudo did not take the password: %w", err)
		}
		e.emit(Event{Type: "diagnostic", Level: "warn", Address: ev.Address, Task: ev.Task, Step: ev.Step, Message: refused.Message})
	}
}

// sudoCommand is what a rule of sudoers allows without a password for the
// programs of the run to run as root.
func (e *engine) sudoCommand() string {
	cmd, err := e.run.SudoCommand()
	if err != nil {
		return "opstruss " + proc.SudoCommand
	}
	return cmd
}

// keepSudo keeps the ticket of sudo alive, from the first program that
// runs as root until the run ends; a rule of sudoers that allows only
// __sudo has no ticket, and its errors go unheard.
func (e *engine) keepSudo() {
	s := &e.sudo
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	go func() {
		t := time.NewTicker(sudoKeep)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = e.run.SudoRefresh(ctx)
			}
		}
	}()
}

// endSudo stops keeping the ticket of sudo, as the run ends.
func (e *engine) endSudo() {
	s := &e.sudo
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
	}
}

// sudoFrom is what the files of f give a program that asks for sudo:
// whether they come from git, and whether the units above them allow it.
// The project, and a frame of none, give neither.
func (f *frame) sudoFrom() (remote, up bool) {
	if f == nil || f.node == nil {
		return false, false
	}
	return f.in.Source != nil, f.node.SudoAllowed()
}

// runsAsRoot reports whether the program of p runs as root: it asks, and
// for the program of a plugin task, what calls the task allows it; in a
// component from git, the units above allow it.
func (p progSpec) runsAsRoot() bool {
	if p.plugin != nil {
		return p.sudo && p.plugin.sudo
	}
	remote, up := p.frame.sudoFrom()
	return model.SudoRuns(p.sudo, remote, up)
}
