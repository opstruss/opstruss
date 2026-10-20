package engine

import (
	"context"
	"strings"
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
		}
	}
	return out, nil
}

// approves says why the pass may not apply a change at addr whose plan has
// digest, or "" when it may: with yes, a unit whose plan this run read is
// approved only when that plan was exact and has the same digest, so what
// is applied is what was seen.
func (e *engine) approves(addr, digest string) string {
	if !e.pass.yes {
		return "needs approval"
	}
	read, ok := e.read[addr]
	switch {
	case !ok:
		return ""
	case read == "":
		return "the plan read earlier in this run was not exact, so yes does not approve it"
	case read != digest:
		return "the plan changed since this run read it"
	}
	return ""
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
