package engine

import (
	"cmp"
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

// sudoKeep is how often a run keeps the ticket of sudo while it lasts.
var sudoKeep = time.Minute

// sudoTries is how many wrong passwords of sudo a run takes, as sudo does.
const sudoTries = 3

// sudoSession is what the programs of one run share of sudo: one question
// for the password at a time, the ticket that sudo keeps once it took
// one, which the run keeps alive until it ends, and the refusals, which
// end the questions for the run.
type sudoSession struct {
	mu     sync.Mutex
	took   int                // the passwords that sudo took, so a program that waited for a question asks none
	wrong  int                // the passwords that sudo did not take
	failed error              // why the run asks no more, once sudo refused
	stop   context.CancelFunc // of the keep-alive, once it runs
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
// fails, with what sudo said. A run asks until sudo took sudoTries wrong
// passwords, or refused the operator, and then no more.
func (e *engine) sudoPassword(ctx context.Context, took int, refused *proc.SudoRefused, ev Event) error {
	s := &e.sudo
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.took != took {
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		// The run stopped while another program had the prompt.
		return errors.New("waiting for the password prompt of sudo")
	}
	if s.failed != nil {
		return s.failed
	}
	if e.opts.Ask == nil {
		// Without a terminal sudo keeps no ticket that this run could share,
		// as in CI, the daemon, and a trigger, so sudoers is the only way.
		return fmt.Errorf("sudo needs a password, and this run has no terminal to ask for it; to run without one, add %s ALL=(root) NOPASSWD: %s to sudoers, which lets any program run as root (%s)",
			username(), e.sudoCommand(), refused.Message)
	}
	// The question names whose program it is, as its block may not show
	// yet.
	label := cmp.Or(e.short(ev.Address), ev.Task)
	for {
		password, err := e.opts.Ask(ctx, fmt.Sprintf("sudo password for %s (%s)", username(), label), &model.Input{Sensitive: true})
		if errors.Is(ctx.Err(), context.Canceled) {
			// The run reports the interrupt; what sudo -v said of it says
			// nothing more.
			return errors.New("at the password prompt of sudo")
		}
		if err != nil {
			return fmt.Errorf("the password of sudo: %w", err)
		}
		e.redact.Add(password)
		if err = e.run.SudoValidate(ctx, password); err == nil {
			s.took++
			return nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.New("at the password prompt of sudo")
		}
		switch {
		case !errors.As(err, &refused):
			return fmt.Errorf("sudo did not take the password: %w", err)
		case !refused.WrongPassword:
			s.failed = fmt.Errorf("sudo refused earlier in this run: %s", refused.Message)
			return fmt.Errorf("sudo refused: %s", refused.Message)
		}
		if s.wrong++; s.wrong == sudoTries {
			s.failed = errors.New("sudo did not take the password earlier in this run")
			return fmt.Errorf("sudo did not take the password after %d attempts", sudoTries)
		}
		e.emit(Event{Type: "diagnostic", Level: "warn", Address: ev.Address, Task: ev.Task, Step: ev.Step, Message: "sudo did not take the password; try again"})
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
