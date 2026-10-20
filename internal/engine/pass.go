package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
func (e *engine) unitPass(ctx context.Context, op string, p pass) (map[string]any, error) {
	e.passMu.Lock()
	defer e.passMu.Unlock()
	e.setInPass(true)
	defer e.setInPass(false)
	e.pass = p
	e.insts, e.byUnit = nil, map[string][]*instance{}
	if op == "down" {
		items, err := e.downPass(ctx)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for _, l := range items {
			out[e.short(l.address)] = map[string]any{"outcome": l.outcome, "changed": l.outcome == success, "outputs": map[string]any{}}
			e.touch(e.short(l.address), &record.Unit{Outcome: l.outcome, Changed: l.outcome == success, Plan: "down"})
		}
		return out, nil
	}
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewMoves()
	e.previewAll()
	leftovers := e.previewLeftovers()
	if len(e.p.Units) == 0 {
		e.say("run", "", "no units")
	}
	// Data that a newer OpsTruss wrote stops the run before any turn, as
	// a newer _meta does, rather than leaving that unit out.
	for _, inst := range e.insts {
		var newer *state.NewerError
		if errors.As(inst.plan.err, &newer) {
			return nil, fmt.Errorf("%s: %w; nothing ran", inst.address, newer)
		}
	}
	out := map[string]any{}
	if op == "plan" {
		for _, inst := range e.insts {
			if e.sel.has(inst) {
				exact := inst.plan.err == nil && inst.plan.waitFor == "" && inst.plan.locked == ""
				digest := ""
				if exact {
					digest = inst.plan.digest
				}
				e.read[inst.address] = digest
				out[e.short(inst.address)] = map[string]any{"outcome": success, "changed": false, "outputs": inst.outputsNow(),
					"plan": inst.plan.view(exact)}
			}
		}
		return out, nil
	}
	e.applyMoves(ctx)
	e.turns(ctx)
	e.settle(ctx, leftovers)
	for _, inst := range e.insts {
		if e.sel.has(inst) {
			out[e.short(inst.address)] = map[string]any{"outcome": inst.outcome, "changed": inst.changed, "outputs": inst.outputsNow(),
				"plan": inst.plan.view(inst.plan.err == nil && inst.plan.waitFor == "" && inst.plan.locked == "")}
			e.touch(e.short(inst.address), &record.Unit{Outcome: inst.outcome, Changed: inst.changed, Plan: inst.plan.summary(), Approval: inst.approval})
		}
	}
	return out, nil
}

// approves says what approves a change at addr whose plan has digest, or
// why nothing does. With yes, a unit whose plan this run read is approved
// only when that plan was exact and has the same digest, so what is
// applied is what was seen; without yes, whoever is at the terminal is
// asked, one question at a time, and the time of the answer does not
// count toward the unit's timeout. It returns how long the question took.
func (e *engine) approves(ctx context.Context, q Question, digest string) (source, why string, waited time.Duration) {
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
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
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

func (e *engine) setInPass(v bool) {
	e.mu.Lock()
	e.inPass = v
	e.mu.Unlock()
}

func (e *engine) short(addr string) string { return strings.TrimPrefix(addr, e.p.StackName+"/") }

func (inst *instance) outputsNow() map[string]any {
	if inst.outputs != nil {
		return inst.outputs
	}
	if inst.record != nil && inst.record.Tombstone == nil && inst.record.Outputs != nil {
		return inst.record.Outputs
	}
	return map[string]any{}
}

// view is units.<n>.plan: one change "run" when up runs, the digest, and
// whether the plan is exact, with the line the command line shows.
func (p plan) view(exact bool) map[string]any {
	changes := []any{}
	if p.run {
		changes = append(changes, map[string]any{"action": "run"})
	}
	for range p.teardown {
		changes = append(changes, map[string]any{"action": "delete"})
	}
	return map[string]any{"changes": changes, "outputs": []any{}, "exact": exact, "digest": p.digest, "summary": p.summary()}
}
