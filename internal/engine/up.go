package engine

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// MaxRecord is the most a record takes; a pending record must have room
// for the largest outputs before its program runs.
const MaxRecord = 2 << 20

// Plan previews the selected units: what up would run, and why.
func Plan(opts Options) (*Result, error) {
	e, err := open(opts)
	if err != nil {
		return nil, err
	}
	defer e.close()
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewAll()
	e.previewLeftovers()
	if len(e.insts) == 0 && len(e.p.Units) == 0 {
		e.say("run", "", "no units")
	}
	return e.result(), nil
}

// Up runs the turn of each selected unit in graph order, units that do not
// depend on each other in parallel, then takes down what left the files.
func Up(ctx context.Context, opts Options) (*Result, error) {
	e, err := open(opts)
	if err != nil {
		return nil, err
	}
	defer e.close()
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewAll()
	leftovers := e.previewLeftovers()
	if len(e.p.Units) == 0 {
		e.say("run", "", "no units")
	}
	e.turns(ctx)
	e.settle(ctx, leftovers)
	return e.result(), nil
}

// previewAll expands and plans every unit in graph order, so a unit knows
// whether this run changes the units it reads before it is planned.
func (e *engine) previewAll() {
	for _, name := range e.p.Order {
		insts, err := e.expand(name)
		if err != nil {
			insts = []*instance{{unit: name, id: name, address: e.p.StackName + "/" + name, def: e.p.Units[name],
				dir: e.p.DirOf("units." + name), done: make(chan struct{}), plan: plan{err: err}}}
		}
		for _, inst := range insts {
			for _, d := range e.p.Deps[name] {
				inst.deps = append(inst.deps, e.byUnit[d]...)
			}
			if err == nil {
				e.preview(inst)
			}
			if e.sel.has(inst) {
				e.say("plan", inst.address, "%s", inst.plan.summary())
			}
		}
		e.byUnit[name] = insts
		e.insts = append(e.insts, insts...)
	}
}

func (e *engine) preview(inst *instance) {
	rec, _, err := e.store.Get(inst.address)
	if err != nil {
		inst.plan.err = err
		return
	}
	e.setRecord(inst, rec)
	if !e.sel.has(inst) {
		return
	}
	if err := tombstoned(rec); err != nil {
		inst.plan.err = err
		return
	}
	token, err := e.store.Lock(inst.address, e.holder)
	if err == nil {
		_ = e.store.Unlock(inst.address, token)
	} else if le, ok := err.(*state.LockedError); ok {
		inst.plan.locked = le.Holder.String()
		return
	} else {
		inst.plan.err = err
		return
	}
	r, err := e.resolve(inst, true)
	switch {
	case err != nil:
		inst.plan.err = err
	case r.waitFor != "":
		inst.plan.waitFor = e.p.StackName + "/" + r.waitFor
	default:
		inst.plan = e.decide(inst, r, rec)
	}
}

func tombstoned(rec *state.Record) error {
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
func (e *engine) decide(inst *instance, r *resolved, rec *state.Record) plan {
	digest, missing, err := e.digest(inst, r)
	if err != nil {
		return plan{err: err}
	}
	p := plan{digest: digest, run: true}
	switch {
	case rec == nil:
		p.reason = "new"
	case rec.Pending != nil:
		p.reason = fmt.Sprintf("%s of %s did not finish", rec.Pending.Op, rec.Pending.Holder)
	case e.forced(inst):
		p.reason = "forced"
	case rec.Digest != digest:
		p.reason = "definition changed"
		if names := changedPass(rec, r); len(names) > 0 {
			p.reason = "context changed: " + strings.Join(names, ", ")
		}
	case len(missing) > 0:
		p.reason = "generates missing: " + strings.Join(missing, ", ")
	default:
		p.run = false
	}
	return p
}

func (e *engine) forced(inst *instance) bool {
	return e.opts.Force && (e.sel.all || e.sel.named[inst.unit])
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
func (e *engine) finish(inst *instance, outcome string, changed bool, outputs map[string]any) {
	e.mu.Lock()
	inst.outcome, inst.changed, inst.outputs = outcome, changed, outputs
	e.mu.Unlock()
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
				e.finish(inst, success, false, nil)
				return
			}
			for _, d := range inst.deps {
				if d.outcome != success {
					e.finish(inst, skipped, false, nil)
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
		e.finish(inst, failure, false, nil)
		e.fail(addr, err)
	}
	if inst.plan.err != nil {
		failed(inst.plan.err)
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
	for _, name := range r.requires {
		if _, err := plugin.LookPath(name, e.path()); err != nil {
			failed(fmt.Errorf("requires: %w", err))
			return
		}
	}
	p := e.decide(inst, r, rec)
	if p.err != nil {
		failed(p.err)
		return
	}
	down := e.downContract(inst, r)
	if !p.run {
		// A down contract that changed alone is written without running.
		if !same(rec.Down, down) {
			rec.Down = down
			if _, err := e.store.Put(addr, rec, etag); err != nil {
				failed(err)
				return
			}
		}
		e.finish(inst, success, false, rec.Outputs)
		e.say("unit", addr, "unchanged")
		return
	}
	approval := cmp.Or(inst.def.Approval, e.stackApproval(), "ask")
	if approval != "auto" {
		if !e.opts.Yes {
			e.finish(inst, skipped, false, nil)
			e.say("approval", addr, "%s; needs approval, stopped at plan", p.summary())
			e.stopAtPlan()
			return
		}
		approval = "--yes"
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
		next.ID = state.NewID()
		next.StateKey = addr + "." + next.ID
	}
	attempt := &state.Attempt{Op: "up", Run: e.runID, Holder: e.holder, Approval: approval, Digest: p.digest,
		Down: down, Needs: e.needs(inst)}
	if prev := next.Pending; prev != nil {
		// What an interrupted attempt parsed stays until this one parses its
		// own, so ids of what it made are not lost if this one stops too.
		attempt.Outputs, attempt.RawOutput = prev.Outputs, prev.RawOutput
	}
	next.Pending = attempt
	if data, err := state.EncodeRecord(next); err != nil || len(data)+plugin.MaxOutputs > MaxRecord {
		failed(fmt.Errorf("the record has no room for outputs of %d bytes", plugin.MaxOutputs))
		return
	}
	failpoint.Hit("up-before-pending")
	if etag, err = e.store.Put(addr, next, etag); err != nil {
		failed(err)
		return
	}
	failpoint.Hit("up-before-program")
	res, err := e.attempts(ctx, inst, r, next, reason, p.digest, func(res *plugin.Result) error {
		next.Pending.Outputs, next.Pending.RawOutput = res.Outputs, ""
		if res.OutputErr != nil {
			next.Pending.RawOutput = string(res.RawOutput)
		}
		etag, err = e.store.Put(addr, next, etag)
		return err
	})
	failpoint.Hit("up-after-program")
	if err != nil {
		failed(err)
		return
	}
	done := &state.Record{ID: next.ID, StateKey: next.StateKey, Kind: "run", Down: down, Needs: attempt.Needs,
		Outputs: res.Outputs, Digest: p.digest, Run: e.runID, Updated: time.Now().UTC().Format(time.RFC3339),
		Abandoned: next.Abandoned}
	failpoint.Hit("up-before-put")
	if _, err := e.store.Put(addr, done, etag); err != nil {
		failed(e.errored(addr, done, err))
		return
	}
	e.finish(inst, success, res.Changed, res.Outputs)
	if res.Changed {
		e.say("unit", addr, "up: done")
	} else {
		e.say("unit", addr, "up: done, nothing changed")
	}
	if _, missing, _ := files(inst.dir, nil, inst.def.Generates); len(missing) > 0 {
		e.warn(addr, "generates still missing after up: %s", strings.Join(missing, ", "))
	}
}

// attempts runs the program as often as retry allows: while an attempt
// fails, or, with until, while until is false. Each attempt that fails is
// kept in the pending record before the next.
func (e *engine) attempts(ctx context.Context, inst *instance, r *resolved, rec *state.Record, reason, digest string,
	keep func(*plugin.Result) error) (*plugin.Result, error) {
	retry := inst.def.Retry
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
			s := e.scope(inst, inst.dir)
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
	case len(res.Sensitive) > 0:
		return fmt.Errorf("the program marked %s sensitive, and sensitive outputs are not supported yet", strings.Join(res.Sensitive, ", "))
	}
	return nil
}

// exec runs the program of up for inst.
func (e *engine) exec(ctx context.Context, inst *instance, r *resolved, rec *state.Record, reason, digest string) (*plugin.Result, error) {
	out := &lineWriter{e: e, addr: inst.address, task: "up", stream: "stdout"}
	errw := &lineWriter{e: e, addr: inst.address, task: "up", stream: "stderr"}
	defer out.flush()
	defer errw.flush()
	res, err := e.run.Exec(ctx, &plugin.Call{Command: r.cmd, Shell: r.shell, Base: inst.dir, Dir: r.dir, Env: r.env,
		Pass: slices.Sorted(maps.Keys(r.pass)), Stdin: r.stdin, Stop: r.stop, Task: "up", Reason: reason, Digest: digest,
		Unit:    &plugin.Unit{Address: inst.address, StateKey: rec.StateKey, Workdir: e.workdir(inst.address), State: programState(rec)},
		Outputs: inst.def.Outputs, Stdout: out, Stderr: errw})
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		e.warn(inst.address, "%s", w)
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

// errored keeps a record that the backend did not take in
// .opstruss/errored/, from where opstruss state push puts it back.
func (e *engine) errored(addr string, rec *state.Record, cause error) error {
	data, err := state.EncodeRecord(rec)
	if err == nil {
		p := filepath.Join(e.root, "errored", filepath.FromSlash(addr)+".json")
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			err = os.WriteFile(p, data, 0o600)
		}
		if err == nil {
			return fmt.Errorf("writing the record: %w; it is kept in %s for opstruss state push", cause, p)
		}
	}
	return fmt.Errorf("writing the record: %w; keeping it failed too: %v", cause, err)
}
