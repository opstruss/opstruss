package engine

import (
	"errors"
	"fmt"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
)

// noMetaError is a scope in a plugin backend without its _meta, which only
// init and state migrate make.
type noMetaError struct{ msg string }

func (e *noMetaError) Error() string { return e.msg }

// StateInit is what InitState did: the _meta it wrote, in the backend at
// Location, which is empty for the local backend.
type StateInit struct {
	Wrote    []string
	Location string
}

// InitState makes the _meta of the scope that opts names, as init does,
// once a plugin backend passed the probe, and writes backend.json there.
// A scope that has _meta keeps it, and one whose state this checkout keeps
// elsewhere is refused, as a run refuses it.
func InitState(opts Options) (*StateInit, error) {
	opts.report, opts.stateOnly = true, true
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	var nm *noMetaError
	if e.originErr != nil && !errors.As(e.originErr, &nm) {
		return nil, e.originErr
	}
	res := &StateInit{Wrote: []string{}}
	pb, plugin := e.store.Backend.(*pluginBackend)
	if plugin {
		res.Location = pb.location
	}
	if e.store.HasMeta() {
		return res, nil
	}
	if plugin {
		if err := state.Probe(pb, state.NewHolder(state.NewRunID(), e.host)); err != nil {
			return nil, fmt.Errorf("backend %s at %s does not keep the contract that the state needs, %w", pb.name, pb.location, err)
		}
	}
	if err := e.store.EnsureMeta(); err != nil {
		return nil, err
	}
	res.Wrote = append(res.Wrote, state.MetaKey(e.p.StackName))
	if plugin {
		if err := e.keepOrigin(e.p.StackName, pb, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// StateHandle is the state of a scope, opened for opstruss state, which
// reads and repairs records without a run. Warning says why the backend
// does not match what this checkout kept the scope in; those commands go
// on, as they are what repairs it.
type StateHandle struct {
	Project *model.Project
	Store   *state.Store
	Warning error
	Close   func()
}

// OpenState opens the state of the scope that opts names in its backend.
func OpenState(opts Options) (*StateHandle, error) {
	opts.report = true
	e, err := load(opts)
	if err != nil {
		return nil, err
	}
	return &StateHandle{Project: e.p, Store: e.store, Warning: e.originErr, Close: e.releaseSources}, nil
}
