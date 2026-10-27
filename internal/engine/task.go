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
	"github.com/opstruss/opstruss/internal/record"
)

// taskResult is how a call of a task or a step ended: its outcome, whether
// it changed something, and its outputs, or one result per key of its
// for_each.
type taskResult struct {
	outcome   string
	changed   bool
	outputs   map[string]any
	approval  bool           // it failed only because something stopped at plan for approval
	units     map[string]any // for a step that calls a task of units: the result of each instance
	keys      map[string]*taskResult
	part      string         // for a block: the outcome of block: alone, which steps.<id>.outcome shows
	soft      bool           // the task failed with continue_on_error, so its caller goes on
	marks     map[string]any // the sensitive outputs
	unitMarks map[string]any // the sensitive outputs of units
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

// viewMarks are the marks of view(): its sensitive outputs.
func (r *taskResult) viewMarks() any {
	if r.keys != nil {
		m := map[string]any{}
		for k, v := range r.keys {
			if vm := v.viewMarks(); vm != nil {
				m[k] = vm
			}
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	m := map[string]any{}
	if len(r.marks) > 0 {
		m["outputs"] = r.marks
	}
	if len(r.unitMarks) > 0 {
		m["units"] = r.unitMarks
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// failed is the result of a call that failed before or outside its
// program; the event carries why.
func failed(error) *taskResult {
	return &taskResult{outcome: failure, outputs: map[string]any{}}
}

// rank orders outcomes for one that stands for several: failure when one
// failed, then cancelled, then skipped, otherwise success.
var rank = map[string]int{success: 0, skipped: 1, cancelled: 2, failure: 3}

// aggregate is the one outcome of several, by rank.
func aggregate(results map[string]*taskResult) *taskResult {
	out := &taskResult{outcome: success, keys: results, approval: true}
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
	yes     bool           // --yes, or yes: true on the step that calls it
	source  string         // what yes is, for the record
	unit    *instance      // the unit, for the task of a unit
	frame   *frame         // the instance of a component whose task it is
	plugin  *pluginRun     // the plugin, for a task of a plugin
	unitRun *unitRun       // what the unit gives, for a task that runs for it
	marks   map[string]any // which of the given inputs are sensitive
	top     bool           // the call of the command line, whose inputs come from its flags
}

// address is the unit that runs the call, which the lines of its programs
// name, or empty.
func (c call) address() string {
	if c.unit != nil {
		return c.unit.address
	}
	return ""
}

// where is the instance of a component whose files write the task, or the
// project.
func (c call) where(e *engine) *frame {
	switch {
	case c.frame != nil:
		return c.frame
	case c.unit != nil:
		return c.unit.frame
	}
	return e.top
}

// envPass is the env and pass that the programs of t start from: its own,
// and for a task of a plugin the env of the call over its own, and the
// variables it asks for that the project lets through.
func (c call) envPass(t *model.Task) (model.Env, []string) {
	if c.plugin == nil {
		return t.Env, t.Pass
	}
	env := maps.Clone(t.Env)
	if env == nil {
		env = model.Env{}
	}
	maps.Copy(env, c.plugin.env)
	return env, c.plugin.pass
}

// Task runs the project task name, or the task name of each unit that has
// it, as one run whose exit code follows the outcome of that call.
func Task(ctx context.Context, opts Options, name string, inputs map[string]any) (*Result, error) {
	e, err := loadContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	e.ctx = ctx
	if err := e.checkCall(name, inputs); err != nil {
		return nil, err
	}
	if err := e.start("task", name, inputs, nil); err != nil {
		return nil, err
	}
	return e.task(e.ctx, name, inputs), nil
}

// task runs the task name in the run that e started.
func (e *engine) task(ctx context.Context, name string, inputs map[string]any) *Result {
	e.rootTask = name
	c := call{yes: e.opts.Yes, source: "--yes", top: !e.trigger}
	var res *taskResult
	if err := e.syncKey(); err != nil {
		e.emit(Event{Type: "task", Level: "error", Task: name, Message: err.Error()})
		res = failed(err)
	} else if t := e.p.Tasks[name]; t != nil {
		if given, err := withFlags(t.Inputs, inputs, e.opts.Flags); err != nil {
			e.emit(Event{Type: "task", Level: "error", Task: name, Message: err.Error()})
			res = failed(err)
		} else {
			res = e.callTask(ctx, name, t, e.p.DirOf("tasks."+name), given, c)
		}
	} else {
		var err error
		if res, err = e.unitTasks(ctx, name, e.opts.Units, inputs, e.opts.Flags, c); err != nil {
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
	e.close(res.outcome, res.outputs, res.marks)
	return e.result()
}

// unitTasks runs the task name of each unit that has it, in graph order,
// without the units' locks; patterns pick the units, and flags, read by the
// inputs of each task, go over inputs.
func (e *engine) unitTasks(ctx context.Context, name string, patterns []string, inputs, flags map[string]any, c call) (*taskResult, error) {
	targets, err := e.unitTargets(name, patterns)
	if err != nil {
		return nil, err
	}
	results := map[string]*taskResult{}
	for _, tg := range targets {
		if tg.frame != nil {
			c.unit, c.frame = nil, tg.frame
		} else {
			c.unit = tg.inst
		}
		given, err := withFlags(tg.task.Inputs, inputs, flags)
		if err != nil {
			return nil, err
		}
		results[tg.key] = e.callTask(ctx, name, tg.task, tg.dir, given, c)
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

// unitTarget is a task of a unit, or of an instance of a component, that a
// call of a task of units reaches; key names its outputs.
type unitTarget struct {
	key   string
	task  *model.Task
	dir   string
	inst  *instance
	frame *frame
}

// unitTargets selects the units of patterns and gives the task name of
// each that has it, in graph order, then those of instances of components.
func (e *engine) unitTargets(name string, patterns []string) ([]unitTarget, error) {
	e.passMu.Lock()
	defer e.passMu.Unlock()
	e.pass = pass{units: patterns, noDeps: true}
	e.resetUnits()
	if err := e.selectUnits(false); err != nil {
		return nil, err
	}
	e.previewQuiet()
	var out []unitTarget
	for _, inst := range e.insts {
		if t := inst.def.Tasks[name]; t != nil && e.sel.has(inst) {
			out = append(out, unitTarget{key: e.short(inst.address), task: t, dir: inst.dir, inst: inst})
		}
	}
	// An instance of a component has the tasks of the component.
	for _, f := range e.allFrames() {
		if t := f.in.Tasks[name]; t != nil && !t.Internal && e.sel.hasFrame(f) {
			out = append(out, unitTarget{key: e.short(f.address), task: t, dir: f.in.DirOf("tasks." + name), frame: f})
		}
	}
	return out, nil
}

// checkCall checks a call of the task name before its run starts: its
// flags, read by the inputs of each task they reach, and for a task of
// units that every unit it reaches has each input given, alike when it
// reaches more than one, so a call that one of them refuses runs none.
func (e *engine) checkCall(name string, inputs map[string]any) error {
	flags := e.opts.Flags
	if t := e.p.Tasks[name]; t != nil {
		_, err := readFlags(t.Inputs, flags)
		return err
	}
	if len(inputs)+len(flags) == 0 {
		return nil
	}
	targets, err := e.unitTargets(name, e.opts.Units)
	e.resetUnits()
	if err != nil {
		return nil // the run reports it
	}
	given := slices.Sorted(maps.Keys(inputs))
	for k := range flags {
		if _, ok := inputs[k]; !ok {
			given = append(given, k)
		}
	}
	slices.Sort(given)
	for _, k := range given {
		for _, tg := range targets {
			switch in := tg.task.Inputs[k]; {
			case in == nil:
				return fmt.Errorf("task %s of %s has no input %s; name the units whose task has it, as in opstruss %s <unit> --%s",
					name, tg.key, k, name, model.FlagName(k))
			case !sameSchema(targets[0].task.Inputs[k], in):
				return fmt.Errorf("input %s of task %s differs in %s and %s; name one of them, as in opstruss %s %s --%s",
					k, name, targets[0].key, tg.key, name, tg.key, model.FlagName(k))
			}
		}
	}
	for _, tg := range targets {
		if _, err := readFlags(tg.task.Inputs, flags); err != nil {
			return err
		}
	}
	return nil
}

// sameSchema reports whether two declarations of an input take the same
// values: their type, items, properties, enum, and sensitive.
func sameSchema(a, b *model.Input) bool {
	view := func(in *model.Input) string {
		return expr.JSON([]any{in.Type, in.Items, in.Properties, in.Enum, in.Sensitive})
	}
	return view(a) == view(b)
}

// withFlags gives the inputs of a call with flags, read by decl, over them.
func withFlags(decl map[string]*model.Input, inputs, flags map[string]any) (map[string]any, error) {
	if len(flags) == 0 {
		return inputs, nil
	}
	read, err := readFlags(decl, flags)
	if err != nil {
		return nil, err
	}
	out := maps.Clone(inputs)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, read)
	return out, nil
}

// readFlags reads flags by decl, the inputs of the task they reach: a text
// by the type of its input, each text of a list by the type of its items,
// and a boolean as it is.
func readFlags(decl map[string]*model.Input, flags map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(flags)) {
		flag := "--" + model.FlagName(k)
		in := decl[k]
		if in == nil {
			return nil, fmt.Errorf("%s: the task has no input %s", flag, k)
		}
		var err error
		switch v := flags[k].(type) {
		case bool:
			out[k] = v
		case string:
			out[k], err = model.ParseText(v, in.Type)
		case []any:
			typ := ""
			if in.Items != nil {
				typ = in.Items.Type
			}
			list := make([]any, len(v))
			for i, item := range v {
				text, ok := item.(string)
				if !ok {
					err = fmt.Errorf("the flag holds %s, not text", expr.JSON(item))
					break
				}
				if list[i], err = model.ParseText(text, typ); err != nil {
					break
				}
			}
			out[k] = list
		default:
			err = fmt.Errorf("the flag holds %s, not text or a boolean", expr.JSON(v))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", flag, err)
		}
	}
	return out, nil
}

// previewQuiet expands the units and reads their records, for the tasks of
// units, which do not plan them.
func (e *engine) previewQuiet() {
	for _, path := range e.p.Order {
		insts, err := e.expand(path)
		if err != nil {
			continue
		}
		for _, inst := range insts {
			if rec, _, err := e.store.Get(inst.address); err == nil {
				inst.record = rec
			}
		}
		e.mu.Lock()
		e.byUnit[path] = insts
		e.mu.Unlock()
		e.insts = append(e.insts, insts...)
	}
	e.expandComponents()
}

// callTask runs one call of a task: its inputs from given and their
// defaults, the tasks it needs, if, for_each, approval, concurrency, then
// its program or steps.
func (e *engine) callTask(ctx context.Context, name string, t *model.Task, dir string, given map[string]any, c call) *taskResult {
	label := name
	switch f := c.where(e); {
	case c.unit != nil:
		label = c.unit.address + "/" + name
	case f.node != nil:
		label = f.address + "/" + name
	}
	fail := func(err error) *taskResult {
		e.emit(Event{Type: "task", Level: "error", Address: c.address(), Task: label, Status: record.Finished, Outcome: failure,
			Message: err.Error()})
		return failed(err)
	}
	decl := t.Inputs
	if c.plugin != nil {
		decl = c.plugin.inputs
	}
	given = maps.Clone(given)
	if given == nil {
		given = map[string]any{}
	}
	if c.plugin == nil {
		// An input without a value reads the environment through its env:,
		// which only the project's own files write.
		for _, k := range slices.Sorted(maps.Keys(t.Inputs)) {
			in := t.Inputs[k]
			if _, ok := given[k]; ok || in.Env == "" {
				continue
			}
			text, ok := e.opts.LookupEnv(in.Env)
			if !ok {
				continue
			}
			if in.Sensitive {
				e.known("input "+k, text)
			}
			v, err := model.ParseText(text, in.Type)
			if err != nil {
				return fail(fmt.Errorf("input %s: %s: %w", k, in.Env, err))
			}
			given[k] = v
		}
	}
	if e.opts.Ask != nil && c.plugin == nil {
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
			if in.Sensitive {
				e.known("input "+k, text)
			}
			v, err := model.ParseText(text, in.Type)
			if err != nil {
				return fail(fmt.Errorf("input %s: %w", k, err))
			}
			given[k] = v
			if c.top {
				e.keepAnswer(k, text)
			}
		}
	}
	// A sensitive value is known before the schema checks it, so the error
	// of the check hides it.
	marks := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(given)) {
		switch {
		case decl[k] != nil && decl[k].Sensitive:
			e.known("input "+k, given[k])
			marks[k] = true
		case c.marks[k] != nil:
			marks[k] = c.marks[k]
		}
	}
	for k, in := range decl {
		if in.Sensitive {
			marks[k] = true
		}
	}
	inputs, err := conform(decl, given)
	if err != nil {
		if c.top && e.opts.RerunOf == "" {
			err = flagHint(decl, given, err)
		}
		return fail(err)
	}
	tasks, taskMarks := map[string]any{}, map[string]any{}
	for _, n := range t.Needs {
		r := e.need(ctx, n, c.where(e))
		tasks[n] = r.view()
		if m := r.viewMarks(); m != nil {
			taskMarks[n] = m
		}
		if r.outcome != success {
			e.endTask(c.address(), label, "", skipped, "skipped: task %s it needs is %s", n, r.outcome)
			return &taskResult{outcome: skipped, outputs: map[string]any{}, approval: r.approval}
		}
	}
	scope := e.taskScope(dir, inputs, marks, tasks, taskMarks, c)
	if c.plugin != nil {
		scope = e.pluginScope(c.plugin.dir, inputs, marks, c.unit)
		if c.plugin.secret {
			// A secret's task writes no artifacts, which the run keeps.
			delete(scope.Vars["opstruss"].(map[string]any), "artifacts")
		}
	}
	if u := c.unitRun; u != nil {
		facts := scope.Vars["opstruss"].(map[string]any)
		facts["state_key"], facts["reason"] = u.stateKey, u.reason
	}
	if err := e.checkRequires(t, c, scope); err != nil {
		return fail(err)
	}
	if t.If != nil {
		ok, err := expr.EvalCondition(t.If, scope)
		if err != nil {
			return fail(fmt.Errorf("if: %w", err))
		}
		if !ok {
			e.endTask(c.address(), label, "", skipped, "skipped: if is false")
			return &taskResult{outcome: skipped, outputs: map[string]any{}}
		}
	}
	if t.Approval == "ask" && !c.yes {
		ok := false
		if e.opts.Approve != nil {
			release := e.asks.wait(-1)
			ok, err = e.opts.Approve(ctx, Question{Op: "task", Task: label, Summary: "run task " + name})
			release()
		}
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			// The run was interrupted while the task waited for its answer.
			e.endTask(c.address(), label, "", cancelled, "interrupted before its approval")
			return &taskResult{outcome: cancelled, outputs: map[string]any{}}
		}
		if !ok || err != nil {
			e.say("approval", label, "task %s needs approval; stopped", name)
			e.stopAtPlan()
			return &taskResult{outcome: skipped, outputs: map[string]any{}, approval: true}
		}
	}
	if c.plugin == nil && c.unitRun == nil {
		// Only a task of the project takes a concurrency lock: a task of a
		// plugin is one operation, and the task that a unit runs is held
		// by the lock of the unit's address, as a run unit is.
		lock := name
		if f := c.where(e); f.node != nil {
			// The task of an instance of a component has a lock of its own.
			lock = e.short(f.address) + "/" + name
		}
		var release func()
		var skip bool
		ctx, release, skip, err = e.taskLock(ctx, lock, t, scope)
		if err != nil {
			return fail(err)
		}
		if skip {
			e.endTask(c.address(), label, "", skipped, "skipped: a run of %s is going on, and concurrency is skip", name)
			return &taskResult{outcome: skipped, outputs: map[string]any{}}
		}
		defer release()
	}
	if t.ForEach == nil {
		return e.taskOnce(ctx, name, label, t, dir, scope, c)
	}
	items, eachMarks, err := e.forEach(t.ForEach, scope)
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
	// Instances start in the order of their keys, so max_parallel: 1 runs
	// them one after another in that order.
	for _, k := range slices.Sorted(maps.Keys(items)) {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := scope.WithMarks("each", map[string]any{"key": k, "value": items[k]}, itemMarks(eachMarks, k))
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
// task that needs it: a task of the project, or of the instance of a
// component f.
func (e *engine) need(ctx context.Context, name string, f *frame) *taskResult {
	key := f.address + " " + name
	e.mu.Lock()
	o := e.needed[key]
	if o == nil {
		o = &taskOnce{}
		e.needed[key] = o
	}
	e.mu.Unlock()
	o.once.Do(func() {
		c := call{yes: e.pass.yes}
		if f.node != nil {
			c.frame = f
		}
		o.res = e.callTask(ctx, name, f.in.Tasks[name], f.in.DirOf("tasks."+name), nil, c)
		if f.node == nil {
			e.ranTask(name, o.res.outcome)
		}
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
	e.startTask(c.address(), label, "")
	var res *taskResult
	env, pass := c.envPass(t)
	switch {
	case t.Run != nil:
		res = e.program(ctx, progSpec{frame: c.where(e), label: label, task: name, cmd: t.Run, shell: t.Shell, dir: t.Dir, base: dir,
			env: env, pass: pass, stdin: t.Stdin, inputs: scope.Vars["inputs"].(map[string]any), outputs: t.Outputs,
			retry: t.Retry, failedWhen: t.FailedWhen, changedWhen: t.ChangedWhen, stop: t.Stop, unit: c.unit, plugin: c.plugin,
			unitRun: c.unitRun}, scope)
	case len(t.Calls) == 1:
		// A task that calls one task passes on its outputs.
		for callee, args := range t.Calls {
			res = e.callByName(ctx, progSpec{frame: c.where(e), label: label, task: name, shell: t.Shell, dir: t.Dir, base: dir, env: env,
				pass: pass, stdin: t.Stdin, retry: t.Retry, failedWhen: t.FailedWhen, changedWhen: t.ChangedWhen,
				stop: t.Stop, unit: c.unit, plugin: c.plugin, unitRun: c.unitRun}, callee, args, scope)
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
			e.endTask(c.address(), label, "", failure, "failed; continue_on_error goes on")
		} else {
			e.endTask(c.address(), label, "", failure, "failed")
		}
	default:
		e.endTask(c.address(), label, "", res.outcome, "%s", res.outcome)
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
		tc := &taskCtx{e: e, name: name, label: label, dir: dir, scope: scope, steps: map[string]any{}, marks: map[string]any{}, c: c}
		env, pass := c.envPass(t)
		res := tc.run(ctx, t.Steps, defaults{shell: t.Shell, dir: t.Dir, env: env, pass: pass}, tc.label+" step ")
		res.outputs, res.marks = tc.values(t.Outputs)
		js := tc.scopeNow().WithMarks("outputs", res.outputs, anyMarks(res.marks))
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
	marks map[string]any // of steps
}

func (tc *taskCtx) scopeNow() *expr.Scope {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	s := tc.scope.WithMarks("steps", maps.Clone(tc.steps), anyMarks(maps.Clone(tc.marks)))
	if tc.c.plugin != nil {
		k := tc.e.newKV()
		return s.WithKV(k.get, k.marked)
	}
	f, k := tc.c.where(tc.e), tc.e.newKV()
	units, unitMarks := tc.e.unitsView(f, slices.Sorted(maps.Keys(f.in.Units)))
	s = s.WithMarks("units", units, anyMarks(unitMarks))
	if f.node == nil {
		return k.scope(s)
	}
	return s.WithMarks("locals", k.localsOf(f, tc.e.inputsOf(f)), s.Marks["locals"]).WithKV(k.get, k.marked)
}

func (tc *taskCtx) record(id string, r *taskResult) {
	if id == "" {
		return
	}
	tc.mu.Lock()
	tc.steps[id] = r.view()
	if m := r.viewMarks(); m != nil {
		tc.marks[id] = m
	} else {
		delete(tc.marks, id)
	}
	tc.mu.Unlock()
}

// values evaluates the outputs of a task with steps: each value: whose
// references exist, and null for the others, so a task that failed still
// gives what can be computed. An output that reads a sensitive value, or
// is declared sensitive, is sensitive.
func (tc *taskCtx) values(decl map[string]*model.Output) (map[string]any, map[string]any) {
	out, marks := map[string]any{}, map[string]any{}
	s := tc.scopeNow()
	for name, o := range decl {
		v, m, err := expr.EvalMarked(o.Value, s)
		if err != nil {
			out[name] = nil
			continue
		}
		out[name] = v
		if m != nil || o.Sensitive {
			marks[name] = true
			tc.e.hide(v, true)
		}
	}
	return out, tc.e.markContent(out, marks, Event{Task: tc.label})
}

// run runs steps in order: a step that fails, unless continue_on_error,
// skips the steps after it in its block or task. A step without a name or
// an id is labeled by where it is, from at: <task> step 2, or <task> step
// 1 rescue 1 for a step in rescue: of the first step.
func (tc *taskCtx) run(ctx context.Context, steps []*model.Step, d defaults, at string) *taskResult {
	res := &taskResult{outcome: success, outputs: map[string]any{}}
	for i, s := range steps {
		label := cmp.Or(s.Name, s.ID, at+strconv.Itoa(i+1))
		if res.outcome != success {
			tc.record(s.ID, &taskResult{outcome: skipped, outputs: map[string]any{}})
			tc.e.endTask(tc.c.address(), tc.label, label, skipped, "skipped")
			continue
		}
		r := tc.step(ctx, s, label, d)
		tc.record(s.ID, r)
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
			return tc.failEnded(label, fmt.Errorf("if: %w", err))
		}
		if !ok {
			tc.e.endTask(tc.c.address(), tc.label, label, skipped, "skipped")
			return &taskResult{outcome: skipped, outputs: map[string]any{}}
		}
	}
	if s.ForEach == nil {
		return tc.stepGroup(ctx, s, label, d, scope)
	}
	items, eachMarks, err := tc.e.forEach(s.ForEach, scope)
	if err != nil {
		return tc.failEnded(label, fmt.Errorf("for_each: %w", err))
	}
	// Each item of for_each is a group of its own, as each instance of a
	// task with for_each is.
	results := map[string]*taskResult{}
	for _, k := range slices.Sorted(maps.Keys(items)) {
		r := tc.stepGroup(ctx, s, label+"["+k+"]", d, scope.WithMarks("each", map[string]any{"key": k, "value": items[k]}, itemMarks(eachMarks, k)))
		results[k] = r
		if r.outcome == failure && !s.ContinueOnError {
			break
		}
	}
	return aggregate(results)
}

// stepGroup runs the body of a step, or of one item of its for_each,
// between the events that start and end it.
func (tc *taskCtx) stepGroup(ctx context.Context, s *model.Step, label string, d defaults, scope *expr.Scope) *taskResult {
	tc.e.startTask(tc.c.address(), tc.label, label)
	res := tc.stepOnce(ctx, s, label, d, scope)
	msg := res.outcome
	switch {
	case res.outcome == failure:
		msg = "failed"
	case res.outcome == success && res.part == failure:
		// rescue: handled what failed in block:, as Ansible counts it
		// rescued; steps.<id>.outcome still says failure.
		msg = "rescued"
	}
	tc.e.endTask(tc.c.address(), tc.label, label, res.outcome, "%s", msg)
	return res
}

func (tc *taskCtx) fail(label string, err error) *taskResult {
	tc.e.emit(Event{Type: "step", Level: "error", Task: tc.label, Step: label, Message: err.Error()})
	return failed(err)
}

// failEnded is fail for a step that err ends before it starts.
func (tc *taskCtx) failEnded(label string, err error) *taskResult {
	tc.e.emit(Event{Type: "step", Level: "error", Address: tc.c.address(), Task: tc.label, Step: label, Status: record.Finished,
		Outcome: failure, Message: err.Error()})
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
		res = tc.e.program(ctx, progSpec{frame: tc.c.where(tc.e), label: tc.label, step: label, task: tc.name, cmd: s.Run, shell: d.shell, dir: d.dir, base: tc.dir,
			env: d.env, pass: d.pass, stdin: s.Stdin, outputs: s.Outputs, retry: s.Retry,
			failedWhen: s.FailedWhen, changedWhen: s.ChangedWhen, unit: tc.c.unit, plugin: tc.c.plugin, unitRun: tc.c.unitRun}, scope)
	case len(s.Calls) == 1:
		d := d.with(s.Shell, s.Dir, s.Env, s.Pass)
		for callee, args := range s.Calls {
			res = tc.e.callByName(ctx, progSpec{frame: tc.c.where(tc.e), label: tc.label, step: label, task: tc.name, shell: d.shell, dir: d.dir,
				base: tc.dir, env: d.env, pass: d.pass, stdin: s.Stdin, retry: s.Retry, failedWhen: s.FailedWhen,
				changedWhen: s.ChangedWhen, unit: tc.c.unit, plugin: tc.c.plugin, unitRun: tc.c.unitRun}, callee, args, scope)
		}
	case s.Task != "":
		res = tc.callStep(ctx, s, label, scope)
		res = judge(res, s.FailedWhen, s.ChangedWhen, scope.WithMarks("outputs", res.outputs, anyMarks(res.marks)))
	case len(s.Block) > 0:
		res = tc.block(ctx, s, label, d.with(s.Shell, s.Dir, s.Env, s.Pass))
	}
	return res
}

// block runs block:, then rescue: when a step of block: failed, then
// always:, also after a timeout or cancel, within stop.grace.
func (tc *taskCtx) block(ctx context.Context, s *model.Step, label string, d defaults) *taskResult {
	res := tc.run(ctx, s.Block, d, label+" block ")
	part := res.outcome
	// rescue: and always: read steps.<id>.outcome as the outcome of block:.
	tc.record(s.ID, &taskResult{outcome: part, changed: res.changed, outputs: map[string]any{}})
	if res.outcome == failure && len(s.Rescue) > 0 {
		r := tc.run(ctx, s.Rescue, d, label+" rescue ")
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
		r := tc.run(actx, s.Always, d, label+" always ")
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

// callStep runs a task: step: plan, up, or destroy of units, a task of the
// project with with: as its inputs, or the task of each unit that has it.
func (tc *taskCtx) callStep(ctx context.Context, s *model.Step, label string, scope *expr.Scope) *taskResult {
	e := tc.e
	yes := tc.c.yes || s.Yes
	source := tc.c.source
	if s.Yes {
		source = fmt.Sprintf("yes: true in task %s step %s", tc.name, cmp.Or(s.ID, label))
	}
	units, err := e.unitPatterns(s.Units, scope)
	if err != nil {
		return tc.fail(label, err)
	}
	f := tc.c.where(e)
	units = e.within(f, units)
	switch s.Task {
	case "plan", "up", "destroy", "status", "logs":
		var got, gotMarks map[string]any
		var err error
		switch s.Task {
		case "status":
			got, gotMarks, err = e.statusPass(ctx, units)
		case "logs":
			got, gotMarks, err = e.logsPass(ctx, units, false)
		default:
			got, gotMarks, err = e.unitPass(ctx, s.Task, pass{units: units, yes: yes, force: s.Force, source: source})
		}
		if err != nil {
			return tc.fail(label, err)
		}
		got = e.relative(f, got)
		res := &taskResult{outcome: success, outputs: map[string]any{}, units: got, approval: true, unitMarks: e.relative(f, gotMarks)}
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
	with, withMarks, err := expr.EvalMarked(anyMap(s.With), scope)
	if err != nil {
		return tc.fail(label, fmt.Errorf("with: %w", err))
	}
	e.hide(with, withMarks)
	given, _ := with.(map[string]any)
	givenMarks, _ := withMarks.(map[string]any)
	c := call{yes: yes, source: source, marks: givenMarks}
	if t := f.in.Tasks[s.Task]; t != nil {
		if f.node != nil {
			c.frame = f
		}
		r := e.callTask(ctx, s.Task, t, f.in.DirOf("tasks."+s.Task), given, c)
		if f.node == nil {
			e.ranTask(s.Task, r.outcome)
		}
		r.soft = t.ContinueOnError
		if r.outcome == skipped && r.approval {
			// What stopped for approval did not run, so the step that called
			// it fails rather than going on as if it had.
			r.outcome = failure
		}
		return r
	}
	r, err := e.unitTasks(ctx, s.Task, units, given, nil, c)
	if err != nil {
		return tc.fail(label, err)
	}
	r.units = e.relative(f, r.units)
	return r
}

// within makes the names of units in a task of an instance of a component
// name the units inside that instance, and no names all of them, so a
// component means the same wherever it is used.
func (e *engine) within(f *frame, patterns []string) []string {
	if f.node == nil {
		return patterns
	}
	at := e.short(f.address)
	if len(patterns) == 0 {
		return []string{at}
	}
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = at + "/" + p
	}
	return out
}

// relative names the units inside the instance of a component f by their
// addresses in it, in results of a task of that instance.
func (e *engine) relative(f *frame, units map[string]any) map[string]any {
	if f.node == nil || units == nil {
		return units
	}
	at := e.short(f.address) + "/"
	out := map[string]any{}
	for k, v := range units {
		out[strings.TrimPrefix(k, at)] = v
	}
	return out
}

// unitPatterns reads units: patterns written in the file stay patterns,
// while the result of a ${{ }} is taken as it is, so data from an event
// cannot pick every instance.
func (e *engine) unitPatterns(list []string, s *expr.Scope) ([]string, error) {
	var out []string
	for _, u := range list {
		if !expr.HasExpr(u) {
			out = append(out, u)
			continue
		}
		v, sensitive, err := expr.EvalTextMarked(u, s)
		if err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
		if sensitive || e.redact.Contains(v) {
			return nil, errors.New("units: a sensitive value is never the name of a unit, which events and records show")
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

// forEach reads the for_each of a task or step: a map, or a list whose
// strings are keys as they are and whose other items are keyed by index;
// with marks for each.value when the items are sensitive. A key shows in
// events and records, so a sensitive value never is one: a sensitive list
// goes through a map with keys of its own.
func (e *engine) forEach(v any, s *expr.Scope) (map[string]any, any, error) {
	val, marks, err := expr.EvalMarked(v, s)
	if err != nil {
		return nil, nil, err
	}
	if marks != nil {
		e.hide(val, marks)
		if _, list := val.([]any); list {
			return nil, nil, errors.New("a sensitive list would make its items keys, which events and records show; give each item a key of its own with a map")
		}
	}
	items, err := forEachItems(val)
	for k := range items {
		if err == nil && e.redact.Contains(k) {
			err = errors.New("a key holds a sensitive value, and keys show in events and records")
		}
	}
	return items, marks, err
}

// itemMarks gives the marks of each for item k of a for_each whose value
// has marks: the whole item when the value is sensitive as a whole, or the
// parts of the item that are, as a map written in YAML keeps them.
func itemMarks(marks any, k string) any {
	var m any
	switch x := marks.(type) {
	case bool:
		if x {
			m = true
		}
	case map[string]any:
		m = x[k]
	}
	if m == nil {
		return nil
	}
	return map[string]any{"value": m}
}

func forEachItems(val any) (map[string]any, error) {
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

// flagHint is err of a call of the command line that left out a required
// input, with how the command line gives it, as gh and kubectl name the
// flag that is missing; a sensitive input takes no flag.
func flagHint(decl map[string]*model.Input, given map[string]any, err error) error {
	for _, k := range slices.Sorted(maps.Keys(decl)) {
		in := decl[k]
		if given[k] != nil || !in.Required || in.Default != nil {
			continue
		}
		flag := model.FlagName(k)
		how := fmt.Sprintf("pass --%s <%s>", flag, cmp.Or(in.Type, "value"))
		switch {
		case in.Sensitive && in.Env != "":
			how = "set $" + in.Env + ", or answer it at a terminal"
		case in.Sensitive:
			how = "answer it at a terminal"
		case in.Type == "boolean":
			how = fmt.Sprintf("pass --%s or --no-%s", flag, flag)
		case in.Type == "array":
			how = fmt.Sprintf("pass --%s <item>, once for each item", flag)
		case in.Type == "object":
			how = fmt.Sprintf("pass --%s with JSON or @<file>", flag)
		}
		return fmt.Errorf("input %s: required; %s", k, how)
	}
	return err
}

// taskScope is what the expressions of a task read: its inputs with those
// of the project, or of the instance of a component whose task it is, and
// the locals and units there; marks and taskMarks say which inputs and
// outputs of tasks are sensitive.
func (e *engine) taskScope(dir string, inputs, marks, tasks, taskMarks map[string]any, c call) *expr.Scope {
	f, k := c.where(e), e.newKV()
	own := e.inputsOf(f)
	all := maps.Clone(own)
	maps.Copy(all, inputs)
	ownMarks := e.inputMarks
	if f.node != nil {
		_, ownMarks = e.inputsAndMarks(f)
	}
	inputMarks := maps.Clone(ownMarks)
	maps.Copy(inputMarks, marks)
	localMarks := e.localMarksOf(f, ownMarks, k)
	facts := e.facts(c.unit)
	if c.unit == nil && f.node != nil {
		facts["address"] = f.address
	}
	facts["dir"], facts["artifacts"], facts["temp"] = dir, e.run.Artifacts, e.run.Temp
	facts["cwd"] = e.opts.Cwd
	units, unitMarks := e.unitsView(f, slices.Sorted(maps.Keys(f.in.Units)))
	s := &expr.Scope{Dir: dir, Vars: map[string]any{"stack": e.p.StackName, "inputs": all, "locals": k.localsOf(f, own),
		"opstruss": facts, "units": units, "tasks": tasks, "steps": map[string]any{}, "secrets": e.secrets, "event": e.event},
		Marks: map[string]any{}}
	if len(e.secrets) > 0 {
		s.Marks["secrets"] = true
	}
	for name, m := range map[string]map[string]any{"inputs": inputMarks, "locals": localMarks, "tasks": taskMarks, "units": unitMarks} {
		if len(m) > 0 {
			s.Marks[name] = m
		}
	}
	return s.WithKV(k.get, k.marked)
}

// anyMarks is m as marks: nil when it marks nothing.
func anyMarks(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
