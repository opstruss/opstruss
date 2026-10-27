package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/state"
)

// leftover is a record that destroy destroys or forgets: in destroy, every
// selected record; in up, an instance that left if or for_each, and a
// unit that removed: decides.
type leftover struct {
	address string
	unit    string   // the path of its unit, as the address names it
	keys    []string // the keys of its address, one per segment
	record  *state.Record
	etag    string // of the record as it was read
	forget  bool
	reason  string
	cause   string      // why, as a word: requested, gone, or removed
	def     *model.Unit // nil for a unit no longer in the files
	auto    bool        // approval: auto, so destroy asks nothing
	done    chan struct{}
	outcome string
}

// summary is the line of plan for a leftover of up.
func (l *leftover) summary() string {
	switch {
	case l.forget:
		return "will be forgotten without being destroyed: removed: with destroy: false"
	case noDestroy(l.record) && l.reason == "requested":
		// Only its record goes, as destroy will say.
		return "will be forgotten: it has no destroy program"
	case noDestroy(l.record):
		return "will be forgotten: " + l.reason + "; it has no destroy program"
	case l.reason == "requested":
		// destroy takes what it was asked to; there is nothing more to say.
		return "will be destroyed"
	}
	return "will be destroyed: " + l.reason
}

// noDestroy reports whether destroy of rec runs no program: it has no destroy
// contract, no attempt left to destroy, and no service to stop.
func noDestroy(rec *state.Record) bool {
	if rec == nil || rec.Destroy != nil || rec.Kind == "service" || len(rec.Abandoned) > 0 {
		return false
	}
	return rec.Pending == nil || rec.Pending.Op != "up" || rec.Pending.Destroy == nil
}

// view is the plan of a leftover of up, as task: plan gives it: exact,
// since nothing is left to learn, with one change, delete or, as
// Terraform calls a removed block without destroy, forget.
func (l *leftover) view() map[string]any {
	action := "delete"
	if l.forget {
		action = "forget"
	}
	v := map[string]any{"changes": []any{map[string]any{"action": action}}, "outputs": []any{}, "exact": true,
		"digest": destroyDigest(action, l.address, l.etag), "summary": l.summary(), "reason": l.cause}
	if l.auto && !l.forget {
		v["approval"] = "auto"
	}
	return v
}

// warnOrphan warns of the record of l, whose unit the files no longer
// have, with the removed: entry that decides what happens to it.
func (e *engine) warnOrphan(l *leftover) {
	e.warn(l.address, "orphan: its unit is no longer in the files; add {from: %s, destroy: true} to removed: to destroy it, or destroy: false to forget it", l.unit)
}

// sayPlan writes the plan line of l, with its plan as units.<n>.plan has
// it.
func (l *leftover) sayPlan(e *engine) {
	e.emit(Event{Type: "plan", Level: "info", Address: l.address, Task: "plan", Message: l.summary(),
		Status: record.Finished, Outcome: success, Plan: l.view()})
}

// destroyDigest is the digest of destroying the record at addr with etag,
// or of forgetting it: the same while neither the operation nor the
// record changes, so yes: true approves only what task: plan showed.
func destroyDigest(action, addr, etag string) string {
	sum := sha256.Sum256([]byte(action + "\n" + addr + "\n" + etag))
	return "sha256:v1:" + hex.EncodeToString(sum[:])
}

// destroyPass destroys the selected units, units that depend on others
// first, each with the destroy contract in its record.
func (e *engine) destroyPass(ctx context.Context) ([]*leftover, error) {
	if err := e.selectUnits(true); err != nil {
		return nil, err
	}
	records, err := e.records(nil)
	if err != nil {
		return nil, err
	}
	var items []*leftover
	for _, l := range records {
		switch {
		case l.record.Kind == "component":
		case l.def == nil:
			if e.sel.all {
				e.warnOrphan(l)
			}
		case e.sel.picked(l.unit, l.keys, false):
			if err := e.checkDestroySources(ctx, l.record); err != nil {
				return nil, err
			}
			l.reason, l.cause = "requested", "requested"
			items = append(items, l)
			l.sayPlan(e)
		}
	}
	if len(records) == 0 {
		e.say("run", "", "stack %s has no units to destroy", e.p.StackName)
	}
	// As Terraform and Pulumi refuse a destroy that reaches a protected
	// resource, nothing is destroyed while one of the units is protected,
	// rather than the units that depend on it going first.
	protected := false
	for _, l := range items {
		if l.def.Protect {
			e.fail(l.address, errors.New("protect: true refuses destroy; remove it from the unit first, or leave the unit out; nothing was destroyed"))
			protected = true
		}
	}
	if !protected {
		e.askFirst(ctx, nil, items)
		e.destroyAll(ctx, items)
		e.dropFrames(ctx, framesOfDestroy(e, items))
	}
	return items, nil
}

// framesOfDestroy are the instances of components that held the items that
// were destroyed or forgotten.
func framesOfDestroy(e *engine, items []*leftover) []string {
	var out []string
	for _, l := range items {
		if l.outcome == success || l.forget {
			out = append(out, e.frameAddresses(l.address)...)
		}
	}
	return out
}

// records reads every record of the stack, tombstones left out.
func (e *engine) records(skip map[string]bool) ([]*leftover, error) {
	addrs, err := e.store.Inventory()
	if err != nil {
		return nil, err
	}
	var out []*leftover
	for _, addr := range addrs {
		if skip[addr] {
			continue
		}
		rec, etag, err := e.store.Get(addr)
		if err != nil {
			return nil, err
		}
		if rec == nil || rec.Tombstone != nil {
			continue
		}
		l := &leftover{address: addr, record: rec, etag: etag, done: make(chan struct{})}
		var names []string
		for _, s := range model.ParseAddress(e.short(addr)) {
			names, l.keys = append(names, s.Unit), append(l.keys, s.Key)
		}
		l.unit = strings.Join(names, "/")
		if n := e.p.Nodes[l.unit]; n != nil && n.Component() == nil && rec.Kind != "component" {
			l.def = n.Def()
		}
		l.auto = e.approvalOf(l.def) == "auto"
		out = append(out, l)
	}
	return out, nil
}

// previewLeftovers finds what up destroys or forgets after the turns:
// instances that if or for_each no longer has, and units that removed:
// decides. Units gone from the files without removed: stay, with a
// warning.
func (e *engine) previewLeftovers() []*leftover {
	planned := map[string]bool{}
	for _, inst := range e.insts {
		planned[inst.address] = true
	}
	records, err := e.records(planned)
	if err != nil {
		e.fail("", err)
		return nil
	}
	current := map[string]bool{}
	for _, fs := range e.frames {
		for _, f := range fs.frames {
			current[f.address] = true
		}
	}
	e.stale = nil
	var out []*leftover
	for _, l := range records {
		if l.record.Kind == "component" && !current[l.address] && !e.moving[l.address] {
			// An instance that this pass did not find drops once nothing
			// inside has a record.
			e.stale = append(e.stale, l.address)
		}
		if planned[l.address] || e.moving[l.address] || l.record.Kind == "component" {
			continue
		}
		rm := e.removal(l)
		switch {
		case l.def != nil:
			if !e.sel.all && len(e.sel.picks[l.unit]) == 0 {
				continue
			}
			if l.def.Protect {
				e.fail(l.address, errors.New("its instance is no longer in if or for_each, and protect: true refuses destroy; it stays"))
				continue
			}
			l.reason, l.cause = "its instance is no longer in if or for_each", "gone"
			l.sayPlan(e)
		case !e.sel.all:
			continue
		case rm == nil:
			e.warnOrphan(l)
			continue
		case rm.Destroy:
			l.reason, l.cause = "removed: with destroy: true", "removed"
			l.sayPlan(e)
		default:
			l.forget, l.cause = true, "removed"
			l.sayPlan(e)
		}
		if !l.forget {
			if err := e.checkDestroySources(e.ctx, l.record); err != nil {
				e.fail(l.address, err)
				continue
			}
		}
		out = append(out, l)
	}
	return out
}

// removal is the entry of removed: that decides for a record whose unit
// left the files: in the files of the innermost component unit around it
// that the files still have, or of the project, the entry of the unit
// under it, which decides for all that unit had inside, in every instance.
func (e *engine) removal(l *leftover) *model.Removed {
	names := strings.Split(l.unit, "/")
	for depth := len(names) - 1; depth >= 0; depth-- {
		in := e.p
		if depth > 0 {
			n := e.p.Nodes[strings.Join(names[:depth], "/")]
			if n == nil || n.Component() == nil {
				continue
			}
			in = n.Component()
		}
		for _, r := range in.Removed {
			if r.From == names[depth] {
				return r
			}
		}
		return nil
	}
	return nil
}

// settle destroys and forgets the leftovers of up, once every turn is
// done, so no unit still reads an instance that is destroyed.
func (e *engine) settle(ctx context.Context, items []*leftover) {
	var destroys []*leftover
	for _, l := range items {
		if !l.forget {
			destroys = append(destroys, l)
			continue
		}
		if err := e.forget(ctx, l.address); err != nil {
			e.failOp(l.address, "destroy", err)
			l.forget, l.outcome = false, failure
			continue
		}
		l.outcome = success
		e.ended(l.address, "destroy", success, "forgotten; a tombstone stays at the address")
	}
	e.destroyAll(ctx, destroys)
	e.dropFrames(ctx, append(framesOfDestroy(e, items), e.stale...))
}

func (e *engine) forget(ctx context.Context, addr string) error {
	token, err := e.lock(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = e.store.Unlock(addr, token) }()
	_, err = e.store.Forget(addr, e.runID)
	return err
}

// destroyAll runs destroy for each item after the items that need it, at most
// OPSTRUSS_PARALLEL at once.
func (e *engine) destroyAll(ctx context.Context, items []*leftover) {
	by := map[string]*leftover{}
	for _, l := range items {
		by[l.address] = l
	}
	needers := map[string][]*leftover{}
	for _, l := range items {
		for _, n := range l.record.Needs {
			if by[n] != nil {
				needers[n] = append(needers[n], l)
			}
		}
	}
	sem := make(chan struct{}, e.parallel)
	var wg sync.WaitGroup
	for _, l := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(l.done)
			for _, n := range needers[l.address] {
				<-n.done
				if n.outcome != success {
					l.outcome = skipped
					e.ended(l.address, "destroy", skipped, "skipped: %s, which needs it, is %s", n.address, n.outcome)
					return
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			l.outcome = e.destroyOne(ctx, l)
		}()
	}
	wg.Wait()
}

// destroyOne destroys one address under its lock: the attempts that did not
// finish first, then the last contract that succeeded, each with the
// context its record holds; then the record goes.
func (e *engine) destroyOne(ctx context.Context, l *leftover) string {
	addr := l.address
	if errors.Is(ctx.Err(), context.Canceled) {
		// The run was interrupted before this turn, as at a question.
		e.ended(addr, "destroy", cancelled, "interrupted before its turn")
		return cancelled
	}
	fail := func(err error) string {
		if errors.Is(ctx.Err(), context.Canceled) {
			// The run was interrupted while the unit was being destroyed, which stopped it.
			e.ended(addr, "destroy", cancelled, "interrupted: %v", err)
			return cancelled
		}
		e.failOp(addr, "destroy", err)
		return failure
	}
	timeout := DefaultTimeout
	if d := l.record.Destroy; d != nil && d.Timeout != "" {
		if t, err := time.ParseDuration(d.Timeout); err == nil {
			timeout = t
		}
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	token, err := e.lock(ctx, addr)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = e.store.Unlock(addr, token) }()
	rec, etag, err := e.store.Get(addr)
	if err != nil {
		return fail(err)
	}
	if rec == nil || rec.Tombstone != nil {
		e.ended(addr, "destroy", success, "already destroy")
		return success
	}
	if err := e.checkDestroySources(ctx, rec); err != nil {
		return fail(err)
	}
	if a := pendingMove(rec); a != nil {
		return fail(fmt.Errorf("this address is moving to %s in run %s; the files with that moved: finish it", a.Peer, a.Run))
	}
	if a := rec.Pending; a != nil && a.Run != e.runID {
		if err := e.waitForPrograms(ctx, addr, a); err != nil {
			return fail(err)
		}
	}
	// An up that did not finish may have made something with its own
	// contract, so that contract is destroyed too, before the last one.
	if a := rec.Pending; a != nil && a.Op == "up" && a.Destroy != nil && !same(a.Destroy, rec.Destroy) {
		rec.Abandoned = append(rec.Abandoned, a)
	}
	contracts := slices.Clone(rec.Abandoned)
	// A service is destroyed by stopping it in its supervisor; its destroy
	// contract says how.
	service := rec.Kind == "service"
	if rec.Destroy != nil && !service {
		contracts = append(contracts, &state.Attempt{Source: rec.Source, Inputs: rec.Inputs, Outputs: rec.Outputs, Destroy: rec.Destroy, Hidden: rec.Hidden})
	}
	if !e.pass.force {
		for _, c := range contracts {
			if names := e.passDiffers(c.Destroy); len(names) > 0 {
				return fail(fmt.Errorf("pass values differ from those of the record: %s; destroy --force runs with the values now", strings.Join(names, ", ")))
			}
		}
	}
	approval := e.approvalOf(l.def)
	if approval != "auto" {
		source, why, _ := e.approves(ctx, Question{Op: "destroy", Address: addr, Summary: l.summary()}, destroyDigest("delete", addr, etag), -1)
		if why != "" && errors.Is(ctx.Err(), context.Canceled) {
			// The run was interrupted while the unit waited for its answer.
			e.ended(addr, "destroy", cancelled, "interrupted before its approval")
			return cancelled
		}
		if why != "" {
			e.stoppedAt(addr, "destroy", "%s; %s, stopped at plan", l.summary(), why)
			e.stopAtPlan()
			return skipped
		}
		approval = source
	}
	e.begin("unit", addr, "destroy", "", "destroy: started")
	rec.Pending = &state.Attempt{Op: "destroy", Run: e.runID, Holder: e.holder, Approval: approval, Source: rec.Source, Destroy: rec.Destroy, Inputs: rec.Inputs, Hidden: rec.Hidden}
	if etag, err = e.store.Put(addr, rec, etag); err != nil {
		return fail(err)
	}
	failpoint.Hit("destroy-before-program")
	if service {
		if err := e.services().Stop(ctx, addr); err != nil {
			return fail(err)
		}
	}
	for len(contracts) > 0 {
		c := contracts[0]
		if err := e.runDestroy(ctx, addr, rec.StateKey, c); err != nil {
			e.endAttempt(addr, rec, etag, parent)
			return fail(err)
		}
		contracts = contracts[1:]
		if len(rec.Abandoned) > 0 {
			rec.Abandoned = rec.Abandoned[1:]
			if etag, err = e.store.Put(addr, rec, etag); err != nil {
				return fail(err)
			}
		}
	}
	failpoint.Hit("destroy-after-program")
	if err := e.store.Delete(addr, etag); err != nil {
		return fail(err)
	}
	// The tool's own state goes with the record it belongs to.
	if err := e.store.RemoveTool(addr); err != nil {
		e.warn(addr, "could not remove the tool's state: %v", err)
	}
	_ = os.RemoveAll(e.workdir(addr))
	switch {
	case service:
		e.ended(addr, "destroy", success, "destroy: stopped")
	case rec.Destroy == nil:
		e.ended(addr, "destroy", success, "forgotten without a destroy program")
	default:
		e.ended(addr, "destroy", success, "destroy: done")
	}
	return success
}

// approvalOf is how a change to a unit of def is approved: by its own
// approval:, that of its stack, or ask.
func (e *engine) approvalOf(def *model.Unit) string {
	own := ""
	if def != nil {
		own = def.Approval
	}
	return cmp.Or(own, e.stackApproval(), "ask")
}

func (e *engine) stackApproval() string {
	if e.p.Stack == nil {
		return ""
	}
	return e.p.Stack.Approval
}

// passDiffers names the variables whose values now differ from the
// fingerprints in a destroy contract, since a destroy that finds nothing at
// another target reports success while the old one keeps running.
func (e *engine) passDiffers(d *state.Destroy) []string {
	if d == nil {
		return nil
	}
	now := map[string]string{}
	for _, kv := range e.opts.Environ {
		k, v, _ := strings.Cut(kv, "=")
		now[k] = v
	}
	var names []string
	for k, fp := range d.Pass {
		if v, ok := now[k]; !ok || fingerprint(v) != fp {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	return names
}

// runDestroy runs one destroy contract, with the inputs and outputs it was
// written with in $OPSTRUSS_STATE.
func (e *engine) runDestroy(ctx context.Context, addr, stateKey string, c *state.Attempt) error {
	d := c.Destroy
	if d == nil {
		return nil
	}
	dir, pluginDir, bins, files, release, err := e.prepareDestroy(ctx, d)
	if err != nil {
		return err
	}
	defer release()
	cmd, err := destroyCommand(d.Run)
	if err != nil {
		return err
	}
	env := map[string]model.EnvValue{}
	for k, v := range d.Env {
		env[k] = model.EnvValue{Text: v}
	}
	for k, v := range d.EnvFiles {
		env[k] = model.EnvValue{Text: v, File: true}
	}
	stop := plugin.DefaultStop
	if d.Stop != nil {
		if stop, err = plugin.StopOf(&model.Stop{Signal: d.Stop.Signal, Grace: d.Stop.Grace}); err != nil {
			return err
		}
	}
	out := &lineWriter{e: e, addr: addr, task: "destroy", stream: "stdout"}
	errw := &lineWriter{e: e, addr: addr, task: "destroy", stream: "stderr"}
	defer out.flush()
	defer errw.flush()
	call := &plugin.Call{Command: cmd, Shell: d.Shell, Base: e.p.Dir, Dir: dir, BinDirs: bins, LeaseFiles: files, Env: env,
		Pass: slices.Sorted(maps.Keys(d.Pass)), Stop: stop, Task: "destroy", Reason: "requested",
		Unit:   &plugin.Unit{Address: addr, StateKey: stateKey, Workdir: e.workdir(addr), State: map[string]any{"inputs": c.Inputs, "outputs": c.Outputs}},
		Stdout: out, Stderr: errw}
	if d.Plugin != "" {
		// destroy of a plugin runs from its directory with the inputs it was
		// written with.
		call.PluginDir, call.Inputs = pluginDir, c.Inputs
		if call.Inputs == nil {
			call.Inputs = map[string]any{}
		}
	}
	if d.Tool {
		call.Unit.StateDir, _ = e.store.ToolDir(addr)
	}
	res, err := e.run.Exec(ctx, call)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		e.warn(addr, "%s", w)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("destroy exited with %d", res.ExitCode)
	}
	return nil
}

// destroyCommand reads the program of a destroy contract: a script, or argv.
func destroyCommand(run any) (model.Command, error) {
	switch x := run.(type) {
	case string:
		return model.Command{Script: x}, nil
	case []any:
		var argv []string
		for _, a := range x {
			s, ok := a.(string)
			if !ok {
				return model.Command{}, fmt.Errorf("the record's destroy program has %v, not a string", a)
			}
			argv = append(argv, s)
		}
		return model.Command{Argv: argv}, nil
	}
	return model.Command{}, fmt.Errorf("the record's destroy program is %T, not a script or a list", run)
}
