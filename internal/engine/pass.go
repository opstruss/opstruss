package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/state"
)

// pass is one plan, up, or down of units in a run: the command line gives
// a run one, and every task: plan, up, or down step another.
type pass struct {
	units  []string
	yes    bool
	force  bool
	noDeps bool
	source string // what approves the pass with yes: --yes, or the step
}

// unitPass runs one pass and returns, for each address it touched without
// its stack, the outcome, changed, outputs, and plan. Passes of a run take
// turns, since they share the instances.
// The marks say which outputs of each unit are sensitive.
func (e *engine) unitPass(ctx context.Context, op string, p pass) (map[string]any, map[string]any, error) {
	e.passMu.Lock()
	defer e.passMu.Unlock()
	e.setInPass(true)
	defer e.setInPass(false)
	e.mu.Lock()
	e.kv = e.newKV()
	e.mu.Unlock()
	e.pass = p
	e.resetUnits()
	marks := map[string]any{}
	if op == "down" {
		items, err := e.downPass(ctx)
		if err != nil {
			return nil, nil, err
		}
		out := map[string]any{}
		for _, l := range items {
			out[e.short(l.address)] = map[string]any{"outcome": l.outcome, "changed": l.outcome == success, "outputs": map[string]any{}}
			e.touch(e.short(l.address), &record.Unit{Outcome: l.outcome, Changed: l.outcome == success, Plan: "down"})
		}
		return out, marks, nil
	}
	if err := e.selectUnits(false); err != nil {
		return nil, nil, err
	}
	e.previewMoves()
	e.setCollect(true)
	e.previewAll(ctx)
	e.setCollect(false)
	leftovers := e.previewLeftovers()
	if len(e.p.Units) == 0 {
		e.say("run", "", "no units")
	}
	// Data that a newer OpsTruss wrote stops the run before any turn, as
	// a newer _meta does, rather than leaving that unit out.
	for _, inst := range e.insts {
		var newer *state.NewerError
		if errors.As(inst.plan.err, &newer) {
			return nil, nil, fmt.Errorf("%s: %w; nothing ran", inst.address, newer)
		}
	}
	out := map[string]any{}
	if op == "plan" {
		for _, inst := range e.insts {
			if e.sel.has(inst) {
				if inst.plan.err != nil {
					// A unit that cannot be planned fails the plan; one that
					// another run holds does not.
					e.exit = max(e.exit, 1)
				}
				exact := inst.plan.err == nil && inst.plan.waitFor == "" && inst.plan.locked == "" && !inst.plan.inexact
				digest := ""
				if exact {
					digest = inst.plan.approval()
				}
				e.read[inst.address] = digest
				out[e.short(inst.address)] = map[string]any{"outcome": success, "changed": false, "outputs": inst.outputsNow(),
					"plan": inst.plan.view(exact)}
				if m := inst.viewMarks(); m != nil {
					marks[e.short(inst.address)] = m
				}
			}
		}
		// What up with the same units takes down or forgets is planned too,
		// so yes: true approves its down only as it was seen.
		for _, l := range leftovers {
			v := l.view()
			e.read[l.address] = v["digest"].(string)
			out[e.short(l.address)] = map[string]any{"outcome": success, "changed": false, "outputs": map[string]any{}, "plan": v}
		}
		return out, marks, nil
	}
	e.applyMoves(ctx)
	e.turns(ctx)
	e.settle(ctx, leftovers)
	for _, inst := range e.insts {
		if e.sel.has(inst) {
			out[e.short(inst.address)] = map[string]any{"outcome": inst.outcome, "changed": inst.changed, "outputs": inst.outputsNow(),
				"plan": inst.plan.view(inst.plan.exact())}
			e.touch(e.short(inst.address), &record.Unit{Outcome: inst.outcome, Changed: inst.changed, Plan: inst.plan.summary(), Approval: inst.approval})
			if m := inst.viewMarks(); m != nil {
				marks[e.short(inst.address)] = m
			}
		}
	}
	// What left if or for_each, or what removed: decides, went down or was
	// forgotten in the same pass.
	for _, l := range leftovers {
		out[e.short(l.address)] = map[string]any{"outcome": l.outcome, "changed": l.outcome == success, "outputs": map[string]any{}, "plan": l.view()}
		e.touch(e.short(l.address), &record.Unit{Outcome: l.outcome, Changed: l.outcome == success, Plan: l.summary()})
	}
	return out, marks, nil
}

// approves says what approves a change at addr whose plan has digest, or
// why nothing does. With yes, a unit whose plan this run read is approved
// only when that plan was exact and has the same digest, so what is
// applied is what was seen; without yes, whoever is at the terminal is
// asked, one question at a time, in the order of place in the plan (see
// askOrder), and the time of the answer does not count toward the unit's
// timeout. It returns how long the question took.
func (e *engine) approves(ctx context.Context, q Question, digest string, place int) (source, why string, waited time.Duration) {
	if e.pass.yes {
		read, ok := e.read[q.Address]
		switch {
		case !ok:
			return e.pass.source, "", 0
		case read == "":
			return "", "the plan read earlier in this run was not exact, so yes does not approve it", 0
		case read != digest:
			return "", "the plan changed since this run read it", 0
		}
		return e.pass.source, "", 0
	}
	if e.opts.Approve == nil {
		return "", "needs approval", 0
	}
	release := e.asks.wait(place)
	defer release()
	start := time.Now()
	ok, err := e.opts.Approve(ctx, q)
	waited = time.Since(start)
	switch {
	case err != nil:
		return "", "no answer: " + err.Error(), waited
	case !ok:
		return "", "the answer was no", waited
	}
	return "terminal", "", waited
}

// askOrder puts the questions of a run at the terminal one at a time, in
// the order of the plan: a turn holds its place from when it starts until
// it asked or knows it will not ask, and a question waits for the places
// before its own. A place below 0, such as that of a task, waits only for
// the terminal.
type askOrder struct {
	mu     sync.Mutex
	cond   *sync.Cond
	places map[int]bool
	asking bool
}

func (o *askOrder) lock() {
	o.mu.Lock()
	if o.cond == nil {
		o.cond, o.places = sync.NewCond(&o.mu), map[int]bool{}
	}
}

// enter holds place until the leave it returns, which may be called more
// than once.
func (o *askOrder) enter(place int) (leave func()) {
	o.lock()
	o.places[place] = true
	o.mu.Unlock()
	return sync.OnceFunc(func() {
		o.lock()
		delete(o.places, place)
		o.cond.Broadcast()
		o.mu.Unlock()
	})
}

// wait waits for the terminal and for the places before place, then holds
// the terminal until the release it returns.
func (o *askOrder) wait(place int) (release func()) {
	o.lock()
	for o.asking || place >= 0 && o.before(place) {
		o.cond.Wait()
	}
	o.asking = true
	o.mu.Unlock()
	return func() {
		o.lock()
		o.asking = false
		o.cond.Broadcast()
		o.mu.Unlock()
	}
}

// before says whether a place before place is held. The caller holds mu.
func (o *askOrder) before(place int) bool {
	for p := range o.places {
		if p < place {
			return true
		}
	}
	return false
}

// setCollect says whether the instances of components that the pass finds
// preview their moves.
func (e *engine) setCollect(v bool) {
	e.mu.Lock()
	e.collect = v
	e.mu.Unlock()
}

func (e *engine) setInPass(v bool) {
	e.mu.Lock()
	e.inPass = v
	e.mu.Unlock()
}

func (e *engine) short(addr string) string { return strings.TrimPrefix(addr, e.p.StackName+"/") }

// outputMarksNow marks the sensitive outputs of outputsNow.
func (inst *instance) outputMarksNow() map[string]any {
	if inst.outputs != nil {
		return inst.outMarks
	}
	if inst.record != nil && inst.record.Tombstone == nil && inst.record.Outputs != nil {
		return hiddenMarks(inst.record.Hidden[state.PartOutputs])
	}
	return nil
}

func (inst *instance) outputsNow() map[string]any {
	if inst.outputs != nil {
		return inst.outputs
	}
	if inst.record != nil && inst.record.Tombstone == nil && inst.record.Outputs != nil {
		return inst.record.Outputs
	}
	return map[string]any{}
}

// exact says whether the plan of an instance is all that the run knows: not
// one that failed, waits for outputs of its turn, or another run holds.
func (p plan) exact() bool {
	return p.err == nil && p.waitFor == "" && p.locked == "" && p.blocked == ""
}

// view is units.<n>.plan: one change "run" when up runs, the digest, and
// whether the plan is exact, with the line the command line shows; and why
// it runs, the parts of the definition that changed, and the unit and the
// parts whose outputs it waits for.
func (p plan) view(exact bool) map[string]any {
	changes := []any{}
	switch {
	case p.changes != nil:
		for _, c := range p.changes {
			changes = append(changes, c)
		}
	case p.run:
		changes = append(changes, map[string]any{"action": "run"})
	}
	for range p.teardown {
		changes = append(changes, map[string]any{"action": "delete"})
	}
	outputs := []any{}
	for _, o := range p.outputs {
		outputs = append(outputs, o)
	}
	v := map[string]any{"changes": changes, "outputs": outputs, "exact": exact && !p.inexact, "digest": p.approval(),
		"summary": p.summary()}
	if p.cause != "" {
		v["reason"] = p.cause
	}
	if len(p.changed) > 0 {
		v["changed"] = anyList(p.changed)
	}
	if p.waitFor != "" {
		v["after"] = p.waitFor
	}
	if len(p.reads) > 0 {
		v["reads"] = anyList(p.reads)
	}
	return v
}

func anyList(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}
