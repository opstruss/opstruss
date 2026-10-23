package engine

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// MaxRecord is the most a record takes; a pending record must have room
// for the largest outputs before its program runs.
const MaxRecord = 2 << 20

// Plan previews the selected units: what up would run, and why.
func Plan(opts Options) (*Result, error) { return runCommand(context.Background(), opts, "plan") }

// Up runs the turn of each selected unit in graph order, units that do not
// depend on each other in parallel, then takes down what left the files.
func Up(ctx context.Context, opts Options) (*Result, error) { return runCommand(ctx, opts, "up") }

// Down takes the selected units down, units that depend on others first,
// each with the down contract in its record.
func Down(ctx context.Context, opts Options) (*Result, error) { return runCommand(ctx, opts, "down") }

func runCommand(ctx context.Context, opts Options, op string) (*Result, error) {
	e, err := open(ctx, opts, op, "")
	if err != nil {
		return nil, err
	}
	if err := e.syncKey(); err != nil {
		e.fail("", err)
	} else if _, _, err := e.unitPass(ctx, op, pass{units: opts.Units, yes: opts.Yes, force: opts.Force, noDeps: opts.NoDeps, source: "--yes"}); err != nil {
		e.fail("", err)
	}
	outcome := map[int]string{0: success, 1: failure, 2: skipped}[e.exit]
	e.close(outcome, nil, nil)
	return e.result(), nil
}

// previewAll expands and plans every unit in graph order, so a unit knows
// whether this run changes the units it reads before it is planned.
func (e *engine) previewAll(ctx context.Context) {
	for _, path := range e.p.Order {
		insts, err := e.expand(path)
		if err != nil {
			insts = []*instance{e.broken(path, err)}
		}
		for _, inst := range insts {
			for _, d := range e.p.Deps[path] {
				inst.deps = append(inst.deps, e.depsOf(inst, d)...)
			}
			if err == nil {
				e.preview(ctx, inst)
			}
			if e.sel.has(inst) {
				e.say("plan", inst.address, "%s", inst.plan.summary())
			}
		}
		e.mu.Lock()
		e.byUnit[path] = insts
		e.mu.Unlock()
		e.insts = append(e.insts, insts...)
	}
	e.expandComponents()
	// Two units that keep the tool's state in one backend overwrite each
	// other's.
	first := map[string]*instance{}
	for _, inst := range e.insts {
		if inst.backend == "" {
			continue
		}
		if other := first[inst.backend]; other != nil {
			err := fmt.Errorf("%s and %s have the same input backend; each unit keeps its own state", other.address, inst.address)
			other.plan, inst.plan = plan{err: err}, plan{err: err}
			for _, i := range []*instance{other, inst} {
				if e.sel.has(i) {
					e.say("plan", i.address, "%s", i.plan.summary())
				}
			}
			continue
		}
		first[inst.backend] = inst
	}
}

// broken stands for the instances of the unit at path, which could not be
// found, at its address without keys.
func (e *engine) broken(path string, err error) *instance {
	n := e.p.Nodes[path]
	return &instance{unit: path, keys: make([]string, strings.Count(path, "/")+1), address: e.p.StackName + "/" + path, node: n,
		frame: e.top, def: n.Def(), dir: n.Dir(), done: make(chan struct{}), plan: plan{err: err}}
}

func (e *engine) preview(ctx context.Context, inst *instance) {
	rec, _, err := e.store.Get(inst.address)
	if err == nil {
		err = e.moveFailed[inst.address]
	}
	if err != nil {
		inst.plan.err = err
		return
	}
	if v := e.moveFrom[inst.address]; v != nil {
		rec = v
	}
	e.setRecord(inst, rec)
	if !e.sel.has(inst) {
		return
	}
	if err := tombstoned(rec); err != nil {
		inst.plan.err = err
		return
	}
	if h, held, err := e.store.Held(inst.address); err != nil {
		inst.plan.err = err
		return
	} else if held {
		inst.plan.locked = h.String()
		return
	}
	r, err := e.resolve(inst, true)
	switch {
	case err != nil:
		inst.plan.err = err
		return
	case r.waitFor != "":
		inst.plan.waitFor = e.p.StackName + "/" + r.waitFor
		return
	}
	if r.plugin != nil && r.plugin.tool != "" {
		if b := r.plugin.inputs["backend"]; b != nil && b != "" {
			inst.backend = expr.JSON(b)
		}
	}
	if err := e.missing(inst, r); err != nil {
		// A unit that runs first in this run may install the program.
		if up := e.runsBefore(inst); up != "" {
			inst.plan.waitFor = up
			e.warn(inst.address, "%v; planned at its turn, after %s", err, up)
			return
		}
		inst.plan.err = err
		return
	}
	if r.plugin != nil && r.plugin.plan != nil {
		// The plan of a plugin reads and may prepare the work directory,
		// so it runs under the lock, which another run may hold.
		token, err := e.store.Lock(inst.address, e.holder)
		var le *state.LockedError
		switch {
		case errors.As(err, &le):
			inst.plan.locked = le.Holder.String()
			return
		case err != nil:
			inst.plan.err = err
			return
		}
		defer func() { _ = e.store.Unlock(inst.address, token) }()
	}
	inst.plan = e.decide(ctx, inst, r, rec)
}

// missing says which program of requires: is not in PATH.
func (e *engine) missing(inst *instance, r *resolved) error {
	for _, name := range r.requires {
		if _, err := plugin.LookPath(name, e.path()); err != nil {
			if r.plugin != nil {
				return fmt.Errorf("plugin %s requires %s, which is not in PATH", r.plugin.target.Package.Name, name)
			}
			return fmt.Errorf("requires: %w", err)
		}
	}
	return nil
}

// runsBefore names an instance that inst depends on and this run runs.
func (e *engine) runsBefore(inst *instance) string {
	for _, d := range inst.deps {
		if (d.plan.run || d.plan.waitFor != "") && e.sel.has(d) {
			return d.address
		}
	}
	return ""
}

// tombstoned says why a unit cannot take the address of rec: a move or a
// forget left it, or an instance of a component has it.
func tombstoned(rec *state.Record) error {
	if rec != nil && rec.Tombstone == nil && rec.Kind == "component" {
		return errors.New("an instance of a component has this address; take what is inside down with the files that still use the component first")
	}
	if rec == nil || rec.Tombstone == nil {
		return nil
	}
	if to := rec.Tombstone.MovedTo; to != "" {
		return fmt.Errorf("this address moved to %s in run %s; these files are older than that move", to, rec.Tombstone.Run)
	}
	return fmt.Errorf("this address was forgotten in run %s; opstruss state rm removes the tombstone so the name can be used again", rec.Tombstone.Run)
}

// decide plans an instance from its record: up runs a new unit, one whose
// last attempt did not finish, one forced, one whose digest changed, and one
// whose generates are missing.
func (e *engine) decide(ctx context.Context, inst *instance, r *resolved, rec *state.Record) plan {
	digest, missing, err := e.digest(inst, r)
	if err != nil {
		return plan{err: err}
	}
	p := plan{digest: digest, run: true}
	if a := pendingMove(rec); a != nil {
		return plan{err: fmt.Errorf("this address is moving to %s in run %s; the files with that moved: finish it", a.Peer, a.Run)}
	}
	if rec != nil {
		// Attempts with the contract up writes become part of the record.
		down := e.downContract(inst, r)
		for _, a := range rec.Abandoned {
			if !sameTarget(a.Down, down) {
				p.teardown = append(p.teardown, a.Run)
			}
		}
		if a := rec.Pending; a != nil && a.Op == "up" && !sameTarget(a.Down, down) {
			p.teardown = append(p.teardown, a.Run)
		}
	}
	switch {
	case rec == nil:
		p.reason = "new"
	case rec.Pending != nil:
		p.reason = fmt.Sprintf("%s of %s did not finish", rec.Pending.Op, rec.Pending.Holder)
	case len(changedPass(rec, r)) > 0:
		// The down of the record reaches the target of the values it
		// recorded; up with others would leave that target without one.
		names := strings.Join(changedPass(rec, r), ", ")
		if !e.forced(inst) {
			return plan{err: fmt.Errorf("context changed: %s differs from the record, whose down reaches the old target; up --force moves the unit and forgets that target", names)}
		}
		p.reason = "context changed: " + names + "; forced, the old target is forgotten"
	case e.forced(inst):
		p.reason = "forced"
	case rec.Digest != digest:
		p.reason = "definition changed"
	case r.service != nil && !e.running(inst.address):
		p.reason = "not running"
	case len(missing) > 0:
		p.reason = "generates missing: " + strings.Join(missing, ", ")
	default:
		p.run = false
	}
	if err := e.toolMoved(inst, r); err != nil {
		return plan{err: err}
	}
	if r.plugin != nil && r.plugin.plan != nil {
		pp, err := e.planPlugin(ctx, inst, r, rec)
		if err != nil {
			return plan{err: err}
		}
		p.changes, p.outputs, p.pluginDigest, p.inexact = pp.Changes, pp.Outputs, pp.Digest, !pp.Exact
		switch {
		case len(pp.Changes) > 0 && !p.run:
			p.run, p.reason = true, "plan: "+describe(pp.Changes)
		case len(pp.Changes) > 0:
			p.reason += "; plan: " + describe(pp.Changes)
		}
	}
	return p
}

func (e *engine) forced(inst *instance) bool {
	return e.pass.force && e.sel.named(inst)
}

// changedPass names the pass variables whose values differ from those
// recorded with down.
func changedPass(rec *state.Record, r *resolved) []string {
	if rec.Down == nil {
		return nil
	}
	var names []string
	for k, fp := range rec.Down.Pass {
		if v, ok := r.pass[k]; !ok || fingerprint(v) != fp {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	return names
}

func (e *engine) setRecord(inst *instance, rec *state.Record) {
	e.mu.Lock()
	inst.record = rec
	e.mu.Unlock()
}

// finish says how an instance ended; units that read it see its outputs.
func (e *engine) finish(inst *instance, outcome string, changed bool, outputs, marks map[string]any) {
	e.mu.Lock()
	inst.outcome, inst.changed, inst.outputs, inst.outMarks = outcome, changed, outputs, marks
	e.mu.Unlock()
}

// outputMarks marks the sensitive outputs of a unit: those it or its
// plugin declares so, those its program names in OPSTRUSS_RESULT, and those
// that hold a sensitive value, which a diagnostic names unless quiet. The
// values of them are hidden from then on.
func (e *engine) outputMarks(inst *instance, r *resolved, res *plugin.Result, quiet bool) map[string]any {
	marks := map[string]any{}
	for k, o := range inst.def.Outputs {
		if o.Sensitive {
			marks[k] = true
		}
	}
	if r.plugin != nil {
		for k, o := range r.plugin.target.Package.Plugin.Outputs.Declared {
			if o.Sensitive {
				marks[k] = true
			}
		}
	}
	for _, k := range res.Sensitive {
		marks[k] = true
	}
	if quiet {
		for k, v := range res.Outputs {
			if e.redact.Contains(v) {
				marks[k] = true
			}
		}
	} else {
		marks = e.markContent(res.Outputs, marks, Event{Address: inst.address, Task: "up"})
	}
	for k := range marks {
		if _, ok := res.Outputs[k]; !ok {
			delete(marks, k)
		}
	}
	e.hide(res.Outputs, anyMarks(marks))
	return marks
}

// withHidden is hidden with the names of the parts in more in place of its
// own.
func withHidden(hidden, more map[string][]string, parts ...string) map[string][]string {
	out := map[string][]string{}
	for p, names := range hidden {
		out[p] = names
	}
	for _, p := range parts {
		if len(more[p]) > 0 {
			out[p] = more[p]
		} else {
			delete(out, p)
		}
	}
	return out
}

// turns runs every instance once the instances it depends on are done, at
// most OPSTRUSS_PARALLEL at once.
func (e *engine) turns(ctx context.Context) {
	sem := make(chan struct{}, e.parallel)
	var wg sync.WaitGroup
	for _, inst := range e.insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(inst.done)
			for _, d := range inst.deps {
				<-d.done
			}
			if !e.sel.has(inst) {
				e.finish(inst, success, false, nil, nil)
				return
			}
			for _, d := range inst.deps {
				if d.outcome != success {
					e.finish(inst, skipped, false, nil, nil)
					e.say("unit", inst.address, "skipped: %s is %s", d.address, d.outcome)
					return
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			e.turn(ctx, inst)
		}()
	}
	wg.Wait()
}

// turn is the turn of one instance: it takes the address lock, plans again
// with what its upstream units gave in this run, asks for approval, marks
// the record pending, runs the program, and writes the record.
func (e *engine) turn(ctx context.Context, inst *instance) {
	addr := inst.address
	failed := func(err error) {
		e.finish(inst, failure, false, nil, nil)
		e.fail(addr, err)
	}
	if inst.plan.err != nil {
		failed(inst.plan.err)
		return
	}
	if err := e.moveFailed[addr]; err != nil {
		failed(err)
		return
	}
	timeout := DefaultTimeout
	if t := inst.def.Timeout; t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			failed(fmt.Errorf("timeout: %w", err))
			return
		}
		timeout = d
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	token, err := e.lock(ctx, addr)
	if err != nil {
		failed(err)
		return
	}
	defer func() { _ = e.store.Unlock(addr, token) }()
	rec, etag, err := e.store.Get(addr)
	if err == nil {
		err = tombstoned(rec)
	}
	if err == nil && rec != nil && rec.Pending != nil && rec.Pending.Run != e.runID {
		err = e.waitForPrograms(ctx, addr, rec.Pending)
	}
	if err != nil {
		failed(err)
		return
	}
	e.setRecord(inst, rec)
	r, err := e.resolve(inst, false)
	if err != nil {
		failed(err)
		return
	}
	if err := e.missing(inst, r); err != nil {
		failed(err)
		return
	}
	p := e.decide(ctx, inst, r, rec)
	if p.err != nil {
		failed(p.err)
		return
	}
	e.debug(addr, "took the lock; digest %s; %s", p.digest, p.summary())
	down := e.downContract(inst, r)
	if !p.run && len(p.teardown) == 0 {
		// A down contract that changed alone is written without running.
		if !same(rec.Down, down) {
			rec.Down = down
			rec.Hidden = withHidden(rec.Hidden, r.hidden(down), state.PartEnv, state.PartEnvFiles)
			if _, err := e.store.Put(addr, rec, etag); err != nil {
				failed(err)
				return
			}
		}
		e.finish(inst, success, false, rec.Outputs, hiddenMarks(rec.Hidden[state.PartOutputs]))
		e.say("unit", addr, "unchanged")
		return
	}
	approval := cmp.Or(inst.def.Approval, e.stackApproval(), "ask")
	if approval != "auto" {
		source, why, waited := e.approves(ctx, Question{Address: addr, Summary: p.summary()}, p.approval())
		if why != "" {
			e.finish(inst, skipped, false, nil, nil)
			e.say("approval", addr, "%s; %s, stopped at plan", p.summary(), why)
			e.stopAtPlan()
			return
		}
		approval = source
		if waited > 0 {
			// Waiting for the answer does not count toward the timeout.
			deadline, _ := ctx.Deadline()
			var more context.CancelFunc
			ctx, more = context.WithDeadline(parent, deadline.Add(waited))
			defer more()
		}
	}
	inst.approval = approval
	if !p.run {
		// Only old attempts are left to tear down; the approval covered them.
		if err := e.tearDown(ctx, addr, rec, etag); err != nil {
			failed(err)
			return
		}
		e.finish(inst, success, true, rec.Outputs, hiddenMarks(rec.Hidden[state.PartOutputs]))
		return
	}
	reason := "changed"
	switch {
	case rec != nil && rec.Pending != nil:
		reason = "interrupted"
	case p.reason == "forced":
		reason = "forced"
	}

	next := &state.Record{}
	if rec != nil {
		*next = *rec
	} else {
		next.ID = e.recordID(inst)
		next.StateKey = addr + "." + next.ID
	}
	attempt := &state.Attempt{Op: "up", Run: e.runID, Holder: e.holder, Approval: approval, Digest: p.digest,
		Down: down, Needs: e.needs(inst), Inputs: r.inputs(), Source: r.source(inst), Hidden: r.hidden(down)}
	if r.sensitive() {
		// What the program writes may hold what it was given.
		attempt.Hidden = withHidden(attempt.Hidden, map[string][]string{state.PartRawOutput: {""}}, state.PartRawOutput)
	}
	if prev := next.Pending; prev != nil {
		if sameTarget(prev.Down, down) {
			// What an interrupted attempt parsed stays until this one parses
			// its own, so ids of what it made are not lost if this one stops
			// too.
			attempt.Outputs, attempt.RawOutput = prev.Outputs, prev.RawOutput
			attempt.Hidden = withHidden(attempt.Hidden, prev.Hidden, state.PartOutputs)
		} else {
			// An attempt with another down contract may have made something
			// elsewhere; it is kept to be torn down after this one succeeds,
			// as Terraform keeps a deposed object.
			next.Abandoned = append(next.Abandoned, prev)
		}
	}
	next.Pending = attempt
	if data, err := e.store.Encode(next); err != nil || len(data)+plugin.MaxOutputs > MaxRecord {
		if err != nil {
			failed(err)
			return
		}
		failed(fmt.Errorf("the record has no room for outputs of %d bytes", plugin.MaxOutputs))
		return
	}
	release, err := e.holdFrames(ctx, inst)
	if err != nil {
		failed(err)
		return
	}
	failpoint.Hit("up-before-pending")
	etag, err = e.store.Put(addr, next, etag)
	release()
	if err != nil {
		failed(err)
		return
	}
	failpoint.Hit("up-before-program")
	// up of a plugin gets the digest of its own plan, to apply what it
	// planned or refuse.
	keep := func(res *plugin.Result, marks map[string]any) error {
		next.Pending.Outputs, next.Pending.RawOutput = res.Outputs, ""
		if res.OutputErr != nil {
			next.Pending.RawOutput = string(res.RawOutput)
		}
		next.Pending.Hidden = withHidden(next.Pending.Hidden, map[string][]string{state.PartOutputs: slices.Sorted(maps.Keys(marks))}, state.PartOutputs)
		tag, err := e.store.Put(addr, next, etag)
		var keyErr *state.KeyError
		if errors.As(err, &keyErr) {
			// An output that the program marked sensitive needs the data key,
			// which the stack cannot have: it is not kept, and the others are,
			// so the next attempt finds what this one made.
			for k := range marks {
				delete(next.Pending.Outputs, k)
			}
			next.Pending.Hidden, next.Pending.RawOutput = withHidden(next.Pending.Hidden, nil, state.PartOutputs), ""
			if tag, err = e.store.Put(addr, next, etag); err == nil {
				err = fmt.Errorf("the program gave sensitive outputs (%s), which were not kept: %w", strings.Join(slices.Sorted(maps.Keys(marks)), ", "), keyErr)
			}
		}
		if tag != "" {
			etag = tag
		}
		return err
	}
	res, err := e.attempts(ctx, inst, r, next, reason, cmp.Or(p.pluginDigest, p.digest), func(res *plugin.Result) error {
		return keep(res, e.outputMarks(inst, r, res, true))
	})
	failpoint.Hit("up-after-program")
	if err != nil {
		failed(err)
		return
	}
	// What the program made is kept in pending first, so a last write
	// that fails still leaves its ids for the next attempt.
	outMarks := e.outputMarks(inst, r, res, false)
	if err := keep(res, outMarks); err != nil {
		failed(err)
		return
	}
	done := &state.Record{ID: next.ID, StateKey: next.StateKey, Kind: r.kind(), Source: attempt.Source, Inputs: attempt.Inputs,
		Down: down, Needs: attempt.Needs, Outputs: res.Outputs, Digest: p.digest, Run: e.runID,
		Updated: time.Now().UTC().Format(time.RFC3339), Abandoned: next.Abandoned,
		Hidden: withHidden(attempt.Hidden, map[string][]string{state.PartOutputs: slices.Sorted(maps.Keys(outMarks))}, state.PartOutputs, state.PartRawOutput)}
	err = failpoint.Fail("up-before-put")
	if err == nil {
		etag, err = e.store.Put(addr, done, etag)
	}
	if err != nil {
		failed(e.errored(addr, done, err))
		return
	}
	if kept := e.erroredPath(addr); fileExists(kept) {
		// A record kept after a write that failed is older than this one.
		_ = os.Remove(kept)
		e.warn(addr, "removed %s, which this up replaces", kept)
	}
	if res.Changed {
		e.say("unit", addr, "up: done")
	} else {
		e.say("unit", addr, "up: done, nothing changed")
	}
	if err := e.tearDown(ctx, addr, done, etag); err != nil {
		failed(err)
		return
	}
	e.finish(inst, success, res.Changed, res.Outputs, outMarks)
	if _, missing, _ := files(inst.dir, nil, inst.def.Generates); len(missing) > 0 {
		e.warn(addr, "generates still missing after up: %s", strings.Join(missing, ", "))
	}
}

// tearDown runs down for each abandoned attempt of rec once up succeeded,
// under the same lock, and drops each one whose down succeeded; one that
// fails stays in the record, and the unit fails. An attempt with the
// contract of the record is dropped without down, since what it made now
// belongs to the record.
func (e *engine) tearDown(ctx context.Context, addr string, rec *state.Record, etag string) error {
	for len(rec.Abandoned) > 0 {
		a := rec.Abandoned[0]
		if sameTarget(a.Down, rec.Down) {
			rec.Abandoned = rec.Abandoned[1:]
			var err error
			if etag, err = e.store.Put(addr, rec, etag); err != nil {
				return err
			}
			e.say("unit", addr, "the attempt of run %s has the contract of the record now; dropped without down", a.Run)
			continue
		}
		if names := e.passDiffers(a.Down); len(names) > 0 {
			return fmt.Errorf("the abandoned attempt of run %s needs %s as it recorded them; run up with those values, or down --force", a.Run, strings.Join(names, ", "))
		}
		if err := e.runDown(ctx, addr, rec.StateKey, a); err != nil {
			return fmt.Errorf("tearing down the attempt of run %s: %w", a.Run, err)
		}
		rec.Abandoned = rec.Abandoned[1:]
		var err error
		if etag, err = e.store.Put(addr, rec, etag); err != nil {
			return err
		}
		e.say("unit", addr, "tore down the attempt of run %s", a.Run)
	}
	return nil
}

// sameTarget reports whether two down contracts take down the same thing:
// the same program, directory, env, and pass values. Timeout and stop do
// not change that.
func sameTarget(a, b *state.Down) bool {
	if a == nil || b == nil {
		return a == b
	}
	return same([]any{a.Run, a.Shell, a.Dir, a.Plugin, a.Env, a.EnvFiles, a.Pass},
		[]any{b.Run, b.Shell, b.Dir, b.Plugin, b.Env, b.EnvFiles, b.Pass})
}

// attempts runs the program as often as retry allows: while an attempt
// fails, or, with until, while until is false. Each attempt that fails is
// kept in the pending record before the next.
func (e *engine) attempts(ctx context.Context, inst *instance, r *resolved, rec *state.Record, reason, digest string,
	keep func(*plugin.Result) error) (*plugin.Result, error) {
	retry := inst.def.Retry
	if r.service != nil {
		// A service restarts by restart:; its start is not repeated.
		retry = nil
	}
	n, delay, backoff := 1, time.Duration(0), 1.0
	if retry != nil {
		n = max(1, retry.Attempts)
		if retry.Delay != "" {
			d, err := time.ParseDuration(retry.Delay)
			if err != nil {
				return nil, fmt.Errorf("retry.delay: %w", err)
			}
			delay = d
		}
		if retry.Backoff > 0 {
			backoff = retry.Backoff
		}
	}
	for i := 1; ; i++ {
		res, err := e.exec(ctx, inst, r, rec, reason, digest)
		if err != nil {
			return nil, err
		}
		ferr := outcomeErr(res)
		done := ferr == nil
		if retry != nil && retry.Until != nil {
			s := e.scope(inst)
			s.Vars["outputs"] = res.Outputs
			ok, err := expr.EvalCondition(retry.Until, s)
			if err != nil {
				return nil, fmt.Errorf("retry.until: %w", err)
			}
			done = ok
		}
		if done && ferr == nil {
			return res, nil
		}
		if err := keep(res); err != nil {
			return nil, err
		}
		if done || i >= n || ctx.Err() != nil {
			if ferr == nil {
				ferr = fmt.Errorf("retry.until is still false after %d attempts", i)
			}
			return nil, ferr
		}
		e.say("unit", inst.address, "attempt %d of %d failed: %v; again in %s", i, n, ferr, delay)
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * backoff)
		reason = "interrupted"
	}
}

// outcomeErr says why a program's result is a failure, or nil.
func outcomeErr(res *plugin.Result) error {
	switch {
	case res.ExitCode != 0:
		return fmt.Errorf("the program exited with %d", res.ExitCode)
	case res.OutputErr != nil:
		return res.OutputErr
	}
	return nil
}

// exec runs the program of up for inst, the task of a unit that uses one,
// or the start of a service.
func (e *engine) exec(ctx context.Context, inst *instance, r *resolved, rec *state.Record, reason, digest string) (*plugin.Result, error) {
	if r.service != nil {
		return e.startService(ctx, inst, r, rec)
	}
	command := r.task != nil && r.task.target.Kind == model.TargetBuiltin && r.task.target.Task == "command"
	if r.task != nil && !command {
		return e.execTask(ctx, inst, r, rec, reason), nil
	}
	out := &lineWriter{e: e, addr: inst.address, task: "up", stream: "stdout"}
	errw := &lineWriter{e: e, addr: inst.address, task: "up", stream: "stderr"}
	defer out.flush()
	defer errw.flush()
	c := &plugin.Call{Command: r.cmd, Shell: r.shell, Base: inst.dir, Dir: r.dir, Env: r.env,
		Pass: slices.Sorted(maps.Keys(r.pass)), Stdin: r.stdin, Stop: r.stop, Task: "up", Reason: reason, Digest: digest,
		Unit: e.unitOf(inst, r, rec), Outputs: inst.def.Outputs, Stdout: out, Stderr: errw}
	if r.plugin != nil {
		outs := r.plugin.target.Package.Plugin.Outputs
		c.Inputs, c.PluginDir, c.Outputs, c.AnyOutputs = r.plugin.inputs, r.plugin.dir, outs.Declared, outs.Tool
	}
	res, err := e.run.Exec(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		e.warn(inst.address, "%s", w)
	}
	logRead(res, c.Outputs, e.outputMarks(inst, r, res, true), out, errw)
	if command {
		// A unit that uses command has its outputs.
		res.Outputs = map[string]any{"stdout": res.Stdout, "stderr": res.Stderr, "exit_code": int64(res.ExitCode)}
	}
	return res, nil
}

// programState is $OPSTRUSS_STATE for up: the inputs and outputs of the
// last attempt that succeeded, the pending one with what it parsed, and the
// abandoned ones.
func programState(rec *state.Record) map[string]any {
	s := map[string]any{"inputs": rec.Inputs, "outputs": rec.Outputs}
	if a := rec.Pending; a != nil {
		s["pending"] = map[string]any{"op": a.Op, "inputs": a.Inputs, "outputs": a.Outputs, "raw_output": a.RawOutput}
	}
	if len(rec.Abandoned) > 0 {
		var list []any
		for _, a := range rec.Abandoned {
			list = append(list, map[string]any{"op": a.Op, "inputs": a.Inputs, "outputs": a.Outputs})
		}
		s["abandoned"] = list
	}
	return s
}

func (e *engine) workdir(addr string) string {
	return filepath.Join(e.root, "work", filepath.FromSlash(addr))
}

// path is the PATH of the run, read once when it started.
func (e *engine) path() string {
	for _, kv := range e.opts.Environ {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

// needs are the addresses inst depends on, which down follows in reverse.
func (e *engine) needs(inst *instance) []string {
	var out []string
	for _, d := range inst.deps {
		out = append(out, d.address)
	}
	return out
}

// same reports whether a and b are the same as JSON, as records store them.
func same(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func (e *engine) erroredPath(addr string) string {
	return filepath.Join(e.root, "errored", filepath.FromSlash(addr)+".json")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// errored keeps a record that the backend did not take in
// .opstruss/errored/, from where opstruss state push puts it back.
func (e *engine) errored(addr string, rec *state.Record, cause error) error {
	data, err := e.store.Encode(rec)
	if err == nil {
		p := e.erroredPath(addr)
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			err = os.WriteFile(p, data, 0o600)
		}
		if err == nil {
			return fmt.Errorf("writing the record: %w; it is kept in %s for opstruss state push", cause, p)
		}
	}
	return fmt.Errorf("writing the record: %w; keeping it failed too: %v", cause, err)
}
