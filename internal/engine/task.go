package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
)

// taskResult is how a call of a task or a step ended: its outcome, whether
// it changed something, and its outputs, or one result per key of its
// for_each.
type taskResult struct {
	outcome  string
	changed  bool
	outputs  map[string]any
	approval bool           // it failed only because something stopped at plan for approval
	units    map[string]any // for a step that calls a task of units: the result of each instance
	keys     map[string]*taskResult
	part     string // for a block: the outcome of block: alone, which steps.<id>.outcome shows
	soft     bool   // the task failed with continue_on_error, so its caller goes on
}

func (r *taskResult) view() any {
	if r.keys != nil {
		m := map[string]any{}
		for k, v := range r.keys {
			m[k] = v.view()
		}
		return m
	}
	v := map[string]any{"outcome": cmp.Or(r.part, r.outcome), "changed": r.changed, "outputs": r.outputs}
	if r.units != nil {
		v["units"] = r.units
	}
	return v
}

// failed is the result of a call that failed before or outside its
// program; the event carries why.
func failed(error) *taskResult {
	return &taskResult{outcome: failure, outputs: map[string]any{}}
}

// aggregate is the one outcome of several: failure when one failed, then
// cancelled, then skipped, otherwise success.
func aggregate(results map[string]*taskResult) *taskResult {
	out := &taskResult{outcome: success, keys: results, approval: true}
	rank := map[string]int{success: 0, skipped: 1, cancelled: 2, failure: 3}
	for _, r := range results {
		if rank[r.outcome] > rank[out.outcome] {
			out.outcome = r.outcome
		}
		out.changed = out.changed || r.changed
		if r.outcome == failure && !r.approval {
			out.approval = false
		}
	}
	if out.outcome != failure && out.outcome != skipped {
		out.approval = false
	}
	return out
}

// call is what the caller of a task gives it besides inputs.
type call struct {
	yes    bool      // --yes, or yes: true on the step that calls it
	source string    // what yes is, for the record
	unit   *instance // the unit, for the task of a unit
}

// Task runs the project task name, or the task name of each unit that has
// it, as one run whose exit code follows the outcome of that call.
func Task(ctx context.Context, opts Options, name string, inputs map[string]any) (*Result, error) {
	e, err := open(opts, "task", name)
	if err != nil {
		return nil, err
	}
	e.rootTask = name
	c := call{yes: opts.Yes, source: "--yes"}
	var res *taskResult
	if t := e.p.Tasks[name]; t != nil {
		res = e.callTask(ctx, name, t, e.p.DirOf("tasks."+name), inputs, c)
	} else {
		if res, err = e.unitTasks(ctx, name, opts.Units, inputs, c); err != nil {
			e.emit(Event{Type: "task", Level: "error", Task: name, Message: err.Error()})
			res = failed(err)
		}
	}
	e.exit = 0
	switch {
	case res.outcome == failure && res.approval, res.outcome == skipped && res.approval:
		e.exit = 2
	case res.outcome == failure, res.outcome == cancelled:
		e.exit = 1
	}
	e.close(res.outcome, res.outputs)
	return &Result{Run: e.runID, ExitCode: e.exit}, nil
}

// unitTasks runs the task name of each unit that has it, in graph order,
// without the units' locks; patterns pick the units.
func (e *engine) unitTasks(ctx context.Context, name string, patterns []string, inputs map[string]any, c call) (*taskResult, error) {
	e.passMu.Lock()
	e.pass = pass{units: patterns, noDeps: true}
	e.insts, e.byUnit = nil, map[string][]*instance{}
	err := e.selectUnits(false)
	if err == nil {
		e.previewQuiet()
	}
	insts := slices.Clone(e.insts)
	e.passMu.Unlock()
	if err != nil {
		return nil, err
	}
	results := map[string]*taskResult{}
	for _, inst := range insts {
		t := inst.def.Tasks[name]
		if t == nil || !e.sel.has(inst) {
			continue
		}
		c.unit = inst
		results[e.short(inst.address)] = e.callTask(ctx, name, t, inst.dir, inputs, c)
	}
	if len(results) == 0 {
		if len(e.p.Units) == 0 {
			e.say("run", "", "no units")
			return &taskResult{outcome: success, outputs: map[string]any{}}, nil
		}
		return nil, fmt.Errorf("there is no task %s in the project or in a unit", name)
	}
	res := aggregate(results)
	res.units = map[string]any{}
	for k, r := range results {
		res.units[k] = r.view()
	}
	res.keys, res.outputs = nil, map[string]any{}
	return res, nil
}

// previewQuiet expands the units and reads their records, for the tasks of
// units, which do not plan them.
func (e *engine) previewQuiet() {
	for _, name := range e.p.Order {
		insts, err := e.expand(name)
		if err != nil {
			continue
		}
		for _, inst := range insts {
			if rec, _, err := e.store.Get(inst.address); err == nil {
				inst.record = rec
			}
		}
		e.byUnit[name] = insts
		e.insts = append(e.insts, insts...)
	}
}

// callTask runs one call of a task: its inputs from given and their
// defaults, the tasks it needs, if, for_each, approval, concurrency, then
// its program or steps.
func (e *engine) callTask(ctx context.Context, name string, t *model.Task, dir string, given map[string]any, c call) *taskResult {
	label := name
	if c.unit != nil {
		label = c.unit.address + " " + name
	}
	fail := func(err error) *taskResult {
		e.emit(Event{Type: "task", Level: "error", Task: label, Message: err.Error()})
		return failed(err)
	}
	if e.opts.Ask != nil {
		// A required input without a value is asked for at the terminal.
		for _, k := range slices.Sorted(maps.Keys(t.Inputs)) {
			in := t.Inputs[k]
			if _, ok := given[k]; ok || !in.Required || in.Default != nil {
				continue
			}
			text, err := e.opts.Ask(ctx, k, in)
			if err != nil {
				return fail(fmt.Errorf("input %s: %w", k, err))
			}
			v, err := model.ParseText(text, in.Type)
			if err != nil {
				return fail(fmt.Errorf("input %s: %w", k, err))
			}
			if given == nil {
				given = map[string]any{}
			}
			given[k] = v
		}
	}
	inputs, err := conform(t.Inputs, given)
	if err != nil {
		return fail(err)
	}
	tasks := map[string]any{}
	for _, n := range t.Needs {
		r := e.need(ctx, n)
		tasks[n] = r.view()
		if r.outcome != success {
			e.sayTask(label, "", "skipped: task %s it needs is %s", n, r.outcome)
			return &taskResult{outcome: skipped, outputs: map[string]any{}, approval: r.approval}
		}
	}
	scope := e.taskScope(dir, inputs, tasks, c.unit)
	if t.If != nil {
		ok, err := expr.EvalCondition(t.If, scope)
		if err != nil {
			return fail(fmt.Errorf("if: %w", err))
		}
		if !ok {
			e.sayTask(label, "", "skipped: if is false")
			return &taskResult{outcome: skipped, outputs: map[string]any{}}
		}
	}
	if t.Approval == "ask" && !c.yes {
		ok := false
		if e.opts.Approve != nil {
			e.promptMu.Lock()
			ok, err = e.opts.Approve(ctx, Question{Task: label, Summary: "run task " + name})
			e.promptMu.Unlock()
		}
		if !ok || err != nil {
			e.say("approval", label, "task %s needs approval; stopped", name)
			e.stopAtPlan()
			return &taskResult{outcome: skipped, outputs: map[string]any{}, approval: true}
		}
	}
	ctx, release, skip, err := e.taskLock(ctx, name, t, scope)
	if err != nil {
		return fail(err)
	}
	if skip {
		e.sayTask(label, "", "skipped: a run of %s is going on, and concurrency is skip", name)
		return &taskResult{outcome: skipped, outputs: map[string]any{}}
	}
	defer release()
	if t.ForEach == nil {
		return e.taskOnce(ctx, name, label, t, dir, scope, c)
	}
	items, err := forEachItems(t.ForEach, scope)
	if err != nil {
		return fail(fmt.Errorf("for_each: %w", err))
	}
	limit := e.parallel
	if t.MaxParallel > 0 {
		limit = t.MaxParallel
	}
	sem := make(chan struct{}, limit)
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := map[string]*taskResult{}
	for _, k := range slices.Sorted(maps.Keys(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s := scope.With("each", map[string]any{"key": k, "value": items[k]})
			r := e.taskOnce(ctx, name, label+"["+k+"]", t, dir, s, c)
			mu.Lock()
			results[k] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return aggregate(results)
}

// need runs a task of needs at most once per run, without inputs from the
// task that needs it.
func (e *engine) need(ctx context.Context, name string) *taskResult {
	e.mu.Lock()
	o := e.needed[name]
	if o == nil {
		o = &taskOnce{}
		e.needed[name] = o
	}
	e.mu.Unlock()
	o.once.Do(func() {
		o.res = e.callTask(ctx, name, e.p.Tasks[name], e.p.DirOf("tasks."+name), nil, call{yes: e.pass.yes})
	})
	return o.res
}

type taskOnce struct {
	once sync.Once
	res  *taskResult
}

// taskOnce runs the body of one instance of a task.
func (e *engine) taskOnce(ctx context.Context, name, label string, t *model.Task, dir string, scope *expr.Scope, c call) *taskResult {
	timeout := DefaultTimeout
	if t.Timeout != "" {
		d, err := time.ParseDuration(t.Timeout)
		if err != nil {
			return failed(err)
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	e.sayTask(label, "", "started")
	var res *taskResult
	switch {
	case t.Run != nil:
		res = e.program(ctx, progSpec{label: label, task: name, cmd: t.Run, shell: t.Shell, dir: t.Dir, base: dir,
			env: t.Env, pass: t.Pass, stdin: t.Stdin, inputs: scope.Vars["inputs"].(map[string]any), outputs: t.Outputs,
			retry: t.Retry, failedWhen: t.FailedWhen, changedWhen: t.ChangedWhen, stop: t.Stop, unit: c.unit}, scope)
	case len(t.Calls) == 1:
		// A task that calls one task passes on its outputs.
		for callee, args := range t.Calls {
			res = e.callByName(ctx, progSpec{label: label, task: name, shell: t.Shell, dir: t.Dir, base: dir, env: t.Env,
				pass: t.Pass, stdin: t.Stdin, retry: t.Retry, failedWhen: t.FailedWhen, changedWhen: t.ChangedWhen,
				stop: t.Stop, unit: c.unit}, callee, args, scope)
		}
	default:
		res = e.stepsWithRetry(ctx, name, label, t, dir, scope, c)
	}
	if ctx.Err() != nil && res.outcome == failure && errors.Is(ctx.Err(), context.Canceled) {
		res.outcome = cancelled
	}
	switch res.outcome {
	case failure:
		if t.ContinueOnError {
			e.sayTask(label, "", "failed; continue_on_error goes on")
		} else {
			e.sayTask(label, "", "failed")
		}
	default:
		e.sayTask(label, "", "%s", res.outcome)
	}
	return res
}

// stepsWithRetry runs the steps of a task, again from the first step as
// retry allows, each attempt judged by failed_when and changed_when with
// the task's outputs as outputs.
func (e *engine) stepsWithRetry(ctx context.Context, name, label string, t *model.Task, dir string, scope *expr.Scope, c call) *taskResult {
	n, delay, backoff := 1, time.Duration(0), 1.0
	if r := t.Retry; r != nil {
		n = max(1, r.Attempts)
		delay, _ = time.ParseDuration(r.Delay)
		if r.Backoff > 0 {
			backoff = r.Backoff
		}
	}
	for i := 1; ; i++ {
		tc := &taskCtx{e: e, name: name, label: label, dir: dir, scope: scope, steps: map[string]any{}, c: c}
		res := tc.run(ctx, t.Steps, defaults{shell: t.Shell, dir: t.Dir, env: t.Env, pass: t.Pass})
		res.outputs = tc.values(t.Outputs)
		js := tc.scopeNow().With("outputs", res.outputs)
		res = judge(res, t.FailedWhen, t.ChangedWhen, js)
		done := res.outcome == success
		if t.Retry != nil && t.Retry.Until != nil {
			ok, err := expr.EvalCondition(t.Retry.Until, js)
			if err != nil {
				res.outcome = failure
				return res
			}
			done = ok
		}
		if done || i >= n || ctx.Err() != nil || res.approval {
			if !done && res.outcome == success {
				res.outcome = failure
			}
			return res
		}
		e.sayTask(label, "", "attempt %d of %d did not succeed; again in %s", i, n, delay)
		select {
		case <-ctx.Done():
			return res
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * backoff)
	}
}

// defaults are shell, dir, env, and pass of a task or block, which each
// step inside overrides key by key.
type defaults struct {
	shell, dir string
	env        model.Env
	pass       []string
}

func (d defaults) with(shell, dir string, env model.Env, pass []string) defaults {
	out := defaults{shell: cmp.Or(shell, d.shell), dir: cmp.Or(dir, d.dir), env: maps.Clone(d.env), pass: d.pass}
	if out.env == nil {
		out.env = model.Env{}
	}
	maps.Copy(out.env, env)
	if pass != nil {
		out.pass = pass
	}
	return out
}

// taskCtx is the running body of a task with steps.
type taskCtx struct {
	e     *engine
	name  string
	label string
	dir   string
	scope *expr.Scope
	c     call

	mu    sync.Mutex
	steps map[string]any
}

func (tc *taskCtx) scopeNow() *expr.Scope {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	s := tc.scope.With("steps", maps.Clone(tc.steps))
	return s.With("units", tc.e.unitsView(tc.e.p.Order))
}

func (tc *taskCtx) record(id string, v any) {
	if id == "" {
		return
	}
	tc.mu.Lock()
	tc.steps[id] = v
	tc.mu.Unlock()
}

// values evaluates the outputs of a task with steps: each value: whose
// references exist, and null for the others, so a task that failed still
// gives what can be computed.
func (tc *taskCtx) values(decl map[string]*model.Output) map[string]any {
	out := map[string]any{}
	s := tc.scopeNow()
	for name, o := range decl {
		v, err := expr.EvalValue(o.Value, s)
		if err != nil {
			out[name] = nil
			continue
		}
		out[name] = v
	}
	return out
}

// run runs steps in order: a step that fails, unless continue_on_error,
// skips the steps after it in its block or task.
func (tc *taskCtx) run(ctx context.Context, steps []*model.Step, d defaults) *taskResult {
	res := &taskResult{outcome: success, outputs: map[string]any{}}
	for i, s := range steps {
		label := cmp.Or(s.Name, s.ID, tc.label+" step "+strconv.Itoa(i+1))
		if res.outcome != success {
			tc.record(s.ID, (&taskResult{outcome: skipped, outputs: map[string]any{}}).view())
			continue
		}
		r := tc.step(ctx, s, label, d)
		tc.record(s.ID, r.view())
		res.changed = res.changed || r.changed
		if r.outcome == failure || r.outcome == cancelled {
			if (s.ContinueOnError || r.soft) && r.outcome == failure {
				tc.e.sayTask(tc.label, label, "failed; continue_on_error goes on")
				continue
			}
			res.outcome, res.approval = r.outcome, r.approval
		}
	}
	return res
}

// step runs one step: if, then for_each in order, each iteration stopping
// the rest when it fails unless continue_on_error.
func (tc *taskCtx) step(ctx context.Context, s *model.Step, label string, d defaults) *taskResult {
	scope := tc.scopeNow()
	if s.If != nil {
		ok, err := expr.EvalCondition(s.If, scope)
		if err != nil {
			return tc.fail(label, fmt.Errorf("if: %w", err))
		}
		if !ok {
			tc.e.sayTask(tc.label, label, "skipped")
			return &taskResult{outcome: skipped, outputs: map[string]any{}}
		}
	}
	if s.ForEach == nil {
		return tc.stepOnce(ctx, s, label, d, scope)
	}
	items, err := forEachItems(s.ForEach, scope)
	if err != nil {
		return tc.fail(label, fmt.Errorf("for_each: %w", err))
	}
	results := map[string]*taskResult{}
	for _, k := range slices.Sorted(maps.Keys(items)) {
		r := tc.stepOnce(ctx, s, label+"["+k+"]", d, scope.With("each", map[string]any{"key": k, "value": items[k]}))
		results[k] = r
		if r.outcome == failure && !s.ContinueOnError {
			break
		}
	}
	return aggregate(results)
}

func (tc *taskCtx) fail(label string, err error) *taskResult {
	tc.e.emit(Event{Type: "step", Level: "error", Task: tc.label, Step: label, Message: err.Error()})
	return failed(err)
}

// stepOnce runs the body of one step and judges it with failed_when,
// changed_when, and retry.
func (tc *taskCtx) stepOnce(ctx context.Context, s *model.Step, label string, d defaults, scope *expr.Scope) *taskResult {
	if s.Timeout != "" {
		t, err := time.ParseDuration(s.Timeout)
		if err != nil {
			return tc.fail(label, err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	var res *taskResult
	switch {
	case s.Run != nil:
		d := d.with(s.Shell, s.Dir, s.Env, s.Pass)
		res = tc.e.program(ctx, progSpec{label: tc.label, step: label, task: tc.name, cmd: s.Run, shell: d.shell, dir: d.dir, base: tc.dir,
			env: d.env, pass: d.pass, stdin: s.Stdin, outputs: s.Outputs, retry: s.Retry,
			failedWhen: s.FailedWhen, changedWhen: s.ChangedWhen, unit: tc.c.unit}, scope)
	case len(s.Calls) == 1:
		d := d.with(s.Shell, s.Dir, s.Env, s.Pass)
		for callee, args := range s.Calls {
			res = tc.e.callByName(ctx, progSpec{label: tc.label, step: label, task: tc.name, shell: d.shell, dir: d.dir,
				base: tc.dir, env: d.env, pass: d.pass, stdin: s.Stdin, retry: s.Retry, failedWhen: s.FailedWhen,
				changedWhen: s.ChangedWhen, unit: tc.c.unit}, callee, args, scope)
		}
	case s.Task != "":
		res = tc.callStep(ctx, s, label, scope)
		res = judge(res, s.FailedWhen, s.ChangedWhen, scope.With("outputs", res.outputs))
	case len(s.Block) > 0:
		res = tc.block(ctx, s, label, d.with(s.Shell, s.Dir, s.Env, s.Pass))
	}
	if res.outcome == failure {
		tc.e.sayTask(tc.label, label, "failed")
	}
	return res
}

// block runs block:, then rescue: when a step of block: failed, then
// always:, also after a timeout or cancel, within stop.grace.
func (tc *taskCtx) block(ctx context.Context, s *model.Step, label string, d defaults) *taskResult {
	res := tc.run(ctx, s.Block, d)
	part := res.outcome
	if res.outcome == failure && len(s.Rescue) > 0 {
		r := tc.run(ctx, s.Rescue, d)
		res.changed = res.changed || r.changed
		res.outcome, res.approval = r.outcome, r.approval
	}
	if len(s.Always) > 0 {
		actx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(context.WithoutCancel(ctx), plugin.DefaultStop.Grace)
			defer cancel()
		}
		r := tc.run(actx, s.Always, d)
		res.changed = res.changed || r.changed
		if r.outcome != success && r.outcome != skipped {
			res.outcome, res.approval = r.outcome, r.approval
		}
	}
	// steps.<block>.outcome is the outcome of block: itself; the sequence
	// goes on when rescue: handled it.
	res.part = part
	return res
}

// callStep runs a task: step: plan, up, or down of units, a task of the
// project with with: as its inputs, or the task of each unit that has it.
func (tc *taskCtx) callStep(ctx context.Context, s *model.Step, label string, scope *expr.Scope) *taskResult {
	e := tc.e
	yes := tc.c.yes || s.Yes
	source := tc.c.source
	if s.Yes {
		source = fmt.Sprintf("yes: true in task %s step %s", tc.name, cmp.Or(s.ID, label))
	}
	units, err := unitPatterns(s.Units, scope)
	if err != nil {
		return tc.fail(label, err)
	}
	switch s.Task {
	case "plan", "up", "down":
		got, err := e.unitPass(ctx, s.Task, pass{units: units, yes: yes, force: s.Force, source: source})
		if err != nil {
			return tc.fail(label, err)
		}
		res := &taskResult{outcome: success, outputs: map[string]any{}, units: got, approval: true}
		for _, v := range got {
			u := v.(map[string]any)
			res.changed = res.changed || u["changed"] == true
			if o := u["outcome"]; o == failure || o == skipped {
				res.outcome = failure
			}
			if u["outcome"] == failure {
				res.approval = false
			}
		}
		if res.outcome == success {
			res.approval = false
		}
		return res
	}
	with, err := expr.EvalValue(anyMap(s.With), scope)
	if err != nil {
		return tc.fail(label, fmt.Errorf("with: %w", err))
	}
	given, _ := with.(map[string]any)
	c := call{yes: yes, source: source}
	if t := e.p.Tasks[s.Task]; t != nil {
		r := e.callTask(ctx, s.Task, t, e.p.DirOf("tasks."+s.Task), given, c)
		r.soft = t.ContinueOnError
		if r.outcome == skipped && r.approval {
			// What stopped for approval did not run, so the step that called
			// it fails rather than going on as if it had.
			r.outcome = failure
		}
		return r
	}
	r, err := e.unitTasks(ctx, s.Task, units, given, c)
	if err != nil {
		return tc.fail(label, err)
	}
	return r
}

// unitPatterns reads units: patterns written in the file stay patterns,
// while the result of a ${{ }} is taken as it is, so data from an event
// cannot pick every instance.
func unitPatterns(list []string, s *expr.Scope) ([]string, error) {
	var out []string
	for _, u := range list {
		if !expr.HasExpr(u) {
			out = append(out, u)
			continue
		}
		v, err := expr.EvalText(u, s)
		if err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
		if strings.Contains(v, "*") {
			return nil, fmt.Errorf("units: %q is not a unit name; patterns are written in the file", v)
		}
		out = append(out, v)
	}
	return out, nil
}

func anyMap(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// judge applies failed_when and changed_when, which read outputs.
func judge(res *taskResult, failedWhen, changedWhen any, s *expr.Scope) *taskResult {
	if failedWhen != nil {
		bad, err := expr.EvalCondition(failedWhen, s)
		switch {
		case err != nil:
			res.outcome = failure
		case bad:
			res.outcome, res.approval = failure, false
		case res.outcome == failure && !res.approval:
			res.outcome = success
		}
	}
	if changedWhen != nil {
		if ch, err := expr.EvalCondition(changedWhen, s); err == nil {
			res.changed = ch
		} else {
			res.outcome = failure
		}
	}
	return res
}

// forEachItems reads the for_each of a task or step: a map, or a list
// whose strings are keys as they are and whose other items are keyed by
// index.
func forEachItems(v any, s *expr.Scope) (map[string]any, error) {
	val, err := expr.EvalValue(v, s)
	if err != nil {
		return nil, err
	}
	items := map[string]any{}
	switch x := val.(type) {
	case map[string]any:
		return x, nil
	case []any:
		for i, it := range x {
			k, ok := it.(string)
			if !ok {
				k = strconv.Itoa(i)
			}
			if _, dup := items[k]; dup {
				return nil, fmt.Errorf("the key %s comes twice", k)
			}
			items[k] = it
		}
		return items, nil
	}
	return nil, fmt.Errorf("takes a list or a map, not %s", model.TypeName(val))
}

// conform gives a task its inputs: each given value checked against its
// schema, and the default of each input not given.
func conform(decl map[string]*model.Input, given map[string]any) (map[string]any, error) {
	for k := range given {
		if decl[k] == nil {
			return nil, fmt.Errorf("the task has no input %s", k)
		}
	}
	out := map[string]any{}
	for k, in := range decl {
		v, err := in.Conform(given[k])
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// taskScope is what the expressions of a task read.
func (e *engine) taskScope(dir string, inputs, tasks map[string]any, unit *instance) *expr.Scope {
	all := maps.Clone(e.inputs)
	maps.Copy(all, inputs)
	facts := e.facts(unit)
	facts["dir"], facts["artifacts"], facts["temp"] = dir, e.run.Artifacts, e.run.Temp
	facts["cwd"] = e.opts.Cwd
	return &expr.Scope{Dir: dir, Vars: map[string]any{"stack": e.p.StackName, "inputs": all, "locals": e.locals,
		"opstruss": facts, "units": e.unitsView(e.p.Order), "tasks": tasks, "steps": map[string]any{}}}
}
