package engine

import (
	"context"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// openGlobal reads, for a run of a stack whose project declares global
// units or tasks, what they read and where their records are: the project
// read for GlobalScope, its inputs without the with: of any stack, its
// locals and secrets, and the store of the scope with its own data key.
// It is an engine that runs nothing itself: the run of the stack expands,
// plans, and runs the global units in its graph, and this one answers
// for their scope, once share gives it the run.
func (e *engine) openGlobal(ctx context.Context) (g *engine, err error) {
	gp, err := model.LoadWith(ctx, e.p.Dir, model.GlobalScope, sources.Options{Locked: e.opts.Locked})
	if err != nil {
		return nil, err
	}
	release := gp.Sources.Retain()
	gp.Close()
	defer func() {
		if err != nil {
			release()
		}
	}()
	// --input of an input that only the stacks give is the stack's.
	given := map[string]string{}
	for k, v := range e.opts.Inputs {
		if gp.Inputs[k] != nil {
			given[k] = v
		}
	}
	values, err := gp.ResolveInputs(given, e.opts.LookupEnv, func(name string, v any) {
		if e.redact.Add(v) {
			e.tooShort = append(e.tooShort, "input "+name)
		}
	})
	if err != nil {
		return nil, e.redact.Err(err)
	}
	opts := e.opts
	opts.Inputs = given
	g = &engine{releaseSources: release, opts: opts, p: gp, inputs: map[string]any{}, root: e.root,
		redact: e.redact, inputMarks: map[string]any{}, started: e.started, parallel: e.parallel, byUnit: map[string][]*instance{},
		read: map[string]string{}, needed: map[string]*taskOnce{}, touched: map[string]*record.Unit{}, ran: map[string]*record.Task{},
		hashes: map[string]string{}, frames: map[string]*frameSet{}, sopsFiles: map[string]any{}, event: e.event, host: e.host,
		backends: e.backends}
	for k, v := range values {
		g.inputs[k] = v.Value
		if gp.Inputs[k].Sensitive {
			e.redact.Add(v.Value)
			g.inputMarks[k] = true
		}
	}
	g.secrets = g.secretValues()
	g.localMarks = gp.LocalMarks(g.projectMarks(g.inputMarks), nil)
	if g.locals, err = g.evalLocals(nil); err != nil {
		return nil, e.redact.Err(err)
	}
	for _, v := range gp.Locals {
		if use, err := expr.AnalyzeValue(v); err == nil && slices.Contains(use.Funcs, "kv") {
			g.kvLocals = true
		}
	}
	g.top = &frame{in: gp, address: model.GlobalScope, global: true}
	// The scopes share a backend that resolves alike: the locks that a run
	// takes in one are released, and waited for, in the same place.
	if g.store, err = g.openStore(gp); err != nil {
		return nil, err
	}
	return g, nil
}

// share gives g, the global scope of the run of e, what the run started:
// its context, record, holder, and programs.
func (g *engine) share(e *engine) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ctx, g.run, g.runID, g.rec, g.holder, g.parents, g.trigger = e.ctx, e.run, e.runID, e.rec, e.holder, e.parents, e.trigger
}

// scopeEngine is the engine that answers for what f reads: the global scope
// for the units in its frame, and e for the others.
func (e *engine) scopeEngine(f *frame) *engine {
	if f != nil && f.global && e.global != nil {
		return e.global
	}
	return e
}

// parentFrames are the frames that n is in: the instances of the
// component around it, or for a unit of the project its scope, which is
// that of GlobalScope for a global unit in a run of a stack.
func (e *engine) parentFrames(n *model.Node) ([]*frame, error) {
	if n.Parent == nil && n.Unit.Global && e.global != nil {
		return []*frame{e.global.top}, nil
	}
	return e.framesOf(n.Parent)
}

// isGlobal says whether addr is in GlobalScope.
func isGlobal(addr string) bool { return strings.HasPrefix(addr, model.GlobalScope+"/") }

// globalDef says whether the unit as it runs is global, as the units inside
// a component that a global unit uses are.
func globalDef(def *model.Unit) bool { return def != nil && def.Global }

// runStack is what a record at addr names as the stack of the run that
// wrote it: the stack of this run for a global unit, whose run is there,
// and nothing otherwise, where the address says it.
func (e *engine) runStack(addr string) string {
	if e.global != nil && isGlobal(addr) {
		return e.p.StackName
	}
	return ""
}

// syncKeys wraps the data keys again for a run that writes state: that of
// its stack, and that of the global scope it reaches.
func (e *engine) syncKeys() error {
	if err := e.syncKey(); err != nil {
		return err
	}
	if e.global != nil {
		return e.global.syncKey()
	}
	return nil
}

// unitRuns are the runs that may have touched the unit at addr, newest
// first: those of its stack, or for a global unit those of every scope,
// since the run of any stack may bring it up.
func (e *engine) unitRuns(addr string) ([]*record.Record, error) {
	if !isGlobal(addr) {
		return runRecords(e.root, e.p.StackName, 0)
	}
	scopes, err := model.Stacks(e.p.Dir)
	if err != nil {
		return nil, err
	}
	var out []*record.Record
	for _, s := range append([]string{model.GlobalScope}, scopes...) {
		runs, err := runRecords(e.root, s, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, runs...)
	}
	slices.SortStableFunc(out, func(a, b *record.Record) int { return strings.Compare(b.Started, a.Started) })
	return out, nil
}

// sameLevel says whether frames a and b hold the units of one file: the
// same instance of a component, or the project, whose global units in a
// run of a stack are in the frame of their scope.
func sameLevel(a, b *frame) bool {
	return a == b || a != nil && b != nil && a.node == nil && b.node == nil
}

// warnStacksNeed warns of the records of other stacks that still need a
// global unit that items destroy: a run destroys the units of its own
// stack that need it first, but never those of another.
func (e *engine) warnStacksNeed(items []*leftover) {
	var globals []*leftover
	for _, l := range items {
		if isGlobal(l.address) {
			globals = append(globals, l)
		}
	}
	if len(globals) == 0 {
		return
	}
	stacks, err := model.Stacks(e.p.Dir)
	if err != nil {
		return
	}
	for _, s := range stacks {
		if s == e.p.StackName {
			continue
		}
		snap, err := (&state.Store{Backend: e.store.Backend, Stack: s}).Snapshot()
		if err != nil {
			continue
		}
		for _, a := range snap.Inventory() {
			rec, err := snap.Get(a)
			if err != nil || rec == nil || rec.Tombstone != nil {
				continue
			}
			for _, l := range globals {
				if recordNeeds(rec, l.address) {
					e.warn(l.address, "%s of stack %s still needs it; take that down first with opstruss destroy --stack %s %s",
						strings.TrimPrefix(a, s+"/"), s, s, strings.TrimPrefix(a, s+"/"))
				}
			}
		}
	}
}

// noUnits says that the project declares no units in the scope of the run.
func (e *engine) noUnits() string {
	if e.p.Global {
		return "the project declares no global units"
	}
	return "the project declares no units"
}
