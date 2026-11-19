package engine

import (
	"errors"
	"fmt"

	"github.com/opstruss/opstruss/internal/state"
)

// Lease is the lease of the daemon of a stack whose state a plugin backend
// keeps: the lock <stack>/_daemon.lock, which one daemon holds while it
// runs, so that of one checkout alone installs the triggers and services
// of a stack that checkouts share. A stack in the local backend needs
// none, since no other checkout shares its state.
type Lease struct {
	release func() error
}

// LeaseError is the lease of a stack that another daemon holds.
type LeaseError struct {
	Stack  string
	Holder state.Holder
}

func (e *LeaseError) Error() string {
	return fmt.Sprintf("the daemon of stack %s on %s holds the lease of the stack, so its triggers and services run there; once that host is gone for good, release it with opstruss unlock _daemon --stack %s",
		e.Stack, e.Holder.Hostname, e.Stack)
}

// TakeLease takes the lease of the daemon of the stack of opts, taking over
// one whose holder died on this host; a *LeaseError names the daemon that
// has it.
func TakeLease(opts Options) (*Lease, error) {
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	pb, ok := e.store.Backend.(*pluginBackend)
	if !ok {
		return &Lease{}, nil
	}
	key := state.DaemonLockKey(e.p.StackName)
	token, err := pb.Lock(key, state.NewHolder(state.DaemonRun, e.host))
	var le *state.LockedError
	switch {
	case errors.As(err, &le):
		return nil, &LeaseError{Stack: e.p.StackName, Holder: le.Holder}
	case err != nil:
		return nil, err
	}
	return &Lease{release: func() error {
		e, err := load(opts)
		if err != nil {
			return err
		}
		defer e.releaseSources()
		return e.store.Backend.Unlock(key, token)
	}}, nil
}

// Release lets the lease go, as the daemon stops.
func (l *Lease) Release() error {
	if l == nil || l.release == nil {
		return nil
	}
	return l.release()
}
