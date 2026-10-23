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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/state"
)

// leftover is a record that down takes down or forgets: in down, every
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
	def     *model.Unit // nil for a unit no longer in the files
	done    chan struct{}
	outcome string
}

// summary is the line of plan for a leftover of up.
func (l *leftover) summary() string {
	if l.forget {
		return "will be forgotten without going down: removed: with destroy: false"
	}
	return "will go down: " + l.reason
}

// view is the plan of a leftover of up, as task: plan gives it: exact,
// since nothing is left to learn, with one change, delete or, as
// Terraform calls a removed block without destroy, forget.
func (l *leftover) view() map[string]any {
	action := "delete"
	if l.forget {
		action = "forget"
	}
	return map[string]any{"changes": []any{map[string]any{"action": action}}, "outputs": []any{}, "exact": true,
		"digest": downDigest(action, l.address, l.etag), "summary": l.summary()}
}

// downDigest is the digest of taking the record at addr with etag down,
// or of forgetting it: the same while neither the operation nor the
// record changes, so yes: true approves only what task: plan showed.
func downDigest(action, addr, etag string) string {
	sum := sha256.Sum256([]byte(action + "\n" + addr + "\n" + etag))
	return "sha256:v1:" + hex.EncodeToString(sum[:])
}

// downPass takes the selected units down, units that depend on others
// first, each with the down contract in its record.
func (e *engine) downPass(ctx context.Context) ([]*leftover, error) {
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
				e.warn(l.address, "orphan: its unit is no longer in the files; decide with removed:")
			}
		case e.sel.picked(l.unit, l.keys, false):
			l.reason = "requested"
			items = append(items, l)
			e.say("plan", l.address, "will go down")
		}
	}
	if len(e.p.Units) == 0 && len(records) == 0 {
		e.say("run", "", "no units")
	}
	// As Terraform and Pulumi refuse a destroy that reaches a protected
	// resource, nothing goes down while one of the units is protected,
	// rather than the units that depend on it going first.
	protected := false
	for _, l := range items {
		if l.def.Protect {
			e.fail(l.address, errors.New("protect: true refuses down; remove it from the unit first, or leave the unit out; nothing went down"))
			protected = true
		}
	}
	if !protected {
		e.downAll(ctx, items)
		e.dropFrames(ctx, framesOfDown(e, items))
	}
	return items, nil
}

// framesOfDown are the instances of components that the items which went
// down or were forgotten were in.
func framesOfDown(e *engine, items []*leftover) []string {
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
		out = append(out, l)
	}
	return out, nil
}

// previewLeftovers finds what up takes down or forgets after the turns:
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
				e.fail(l.address, errors.New("its instance is no longer in if or for_each, and protect: true refuses down; it stays"))
				continue
			}
			l.reason = "its instance is no longer in if or for_each"
			e.say("plan", l.address, "%s", l.summary())
		case !e.sel.all:
			continue
		case rm == nil:
			e.warn(l.address, "orphan: its unit is no longer in the files; decide with removed:")
			continue
		case rm.Destroy:
			l.reason = "removed: with destroy: true"
			e.say("plan", l.address, "%s", l.summary())
		default:
			l.forget = true
			e.say("plan", l.address, "%s", l.summary())
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

// settle takes down and forgets the leftovers of up, once every turn is
// done, so no unit still reads an instance that goes down.
func (e *engine) settle(ctx context.Context, items []*leftover) {
	var downs []*leftover
	for _, l := range items {
		if !l.forget {
			downs = append(downs, l)
			continue
		}
		if err := e.forget(ctx, l.address); err != nil {
			e.fail(l.address, err)
			l.forget, l.outcome = false, failure
			continue
		}
		l.outcome = success
		e.say("unit", l.address, "forgotten; a tombstone stays at the address")
	}
	e.downAll(ctx, downs)
	e.dropFrames(ctx, append(framesOfDown(e, items), e.stale...))
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

// downAll runs down for each item after the items that need it, at most
// OPSTRUSS_PARALLEL at once.
func (e *engine) downAll(ctx context.Context, items []*leftover) {
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
					e.say("unit", l.address, "skipped: %s, which needs it, is %s", n.address, n.outcome)
					return
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			l.outcome = e.downOne(ctx, l)
		}()
	}
	wg.Wait()
}

// downOne takes one address down under its lock: the attempts that did not
// finish first, then the last contract that succeeded, each with the
// context its record holds; then the record goes.
func (e *engine) downOne(ctx context.Context, l *leftover) string {
	addr := l.address
	fail := func(err error) string {
		e.fail(addr, err)
		return failure
	}
	timeout := DefaultTimeout
	if d := l.record.Down; d != nil && d.Timeout != "" {
		if t, err := time.ParseDuration(d.Timeout); err == nil {
			timeout = t
		}
	}
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
		e.say("unit", addr, "already down")
		return success
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
	// contract, so that contract is torn down too, before the last one.
	if a := rec.Pending; a != nil && a.Op == "up" && a.Down != nil && !same(a.Down, rec.Down) {
		rec.Abandoned = append(rec.Abandoned, a)
	}
	contracts := slices.Clone(rec.Abandoned)
	// A service goes down when its supervisor stops it; its down contract
	// says how.
	service := rec.Kind == "service"
	if rec.Down != nil && !service {
		contracts = append(contracts, &state.Attempt{Inputs: rec.Inputs, Outputs: rec.Outputs, Down: rec.Down, Hidden: rec.Hidden})
	}
	if !e.pass.force {
		for _, c := range contracts {
			if names := e.passDiffers(c.Down); len(names) > 0 {
				return fail(fmt.Errorf("pass values differ from those of the record: %s; down --force runs with the values now", strings.Join(names, ", ")))
			}
		}
	}
	approval := cmp.Or(e.stackApproval(), "ask")
	if l.def != nil {
		approval = cmp.Or(l.def.Approval, approval)
	}
	if approval != "auto" {
		source, why, _ := e.approves(ctx, Question{Address: addr, Summary: "will go down (" + l.reason + ")"}, downDigest("delete", addr, etag))
		if why != "" {
			e.say("approval", addr, "will go down (%s); %s, stopped at plan", l.reason, why)
			e.stopAtPlan()
			return skipped
		}
		approval = source
	}
	rec.Pending = &state.Attempt{Op: "down", Run: e.runID, Holder: e.holder, Approval: approval}
	if etag, err = e.store.Put(addr, rec, etag); err != nil {
		return fail(err)
	}
	failpoint.Hit("down-before-program")
	if service {
		if err := e.services().Stop(ctx, addr); err != nil {
			return fail(err)
		}
	}
	for len(contracts) > 0 {
		c := contracts[0]
		if err := e.runDown(ctx, addr, rec.StateKey, c); err != nil {
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
	failpoint.Hit("down-after-program")
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
		e.say("unit", addr, "down: stopped")
	case rec.Down == nil:
		e.say("unit", addr, "forgotten without a down program")
	default:
		e.say("unit", addr, "down: done")
	}
	return success
}

func (e *engine) stackApproval() string {
	if e.p.Stack == nil {
		return ""
	}
	return e.p.Stack.Approval
}

// passDiffers names the variables whose values now differ from the
// fingerprints in a down contract, since a down that finds nothing at
// another target reports success while the old one keeps running.
func (e *engine) passDiffers(d *state.Down) []string {
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

// runDown runs one down contract, with the inputs and outputs it was
// written with in $OPSTRUSS_STATE.
func (e *engine) runDown(ctx context.Context, addr, stateKey string, c *state.Attempt) error {
	d := c.Down
	if d == nil {
		return nil
	}
	cmd, err := downCommand(d.Run)
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
	out := &lineWriter{e: e, addr: addr, task: "down", stream: "stdout"}
	errw := &lineWriter{e: e, addr: addr, task: "down", stream: "stderr"}
	defer out.flush()
	defer errw.flush()
	call := &plugin.Call{Command: cmd, Shell: d.Shell, Base: e.p.Dir, Dir: d.Dir, Env: env,
		Pass: slices.Sorted(maps.Keys(d.Pass)), Stop: stop, Task: "down", Reason: "requested",
		Unit:   &plugin.Unit{Address: addr, StateKey: stateKey, Workdir: e.workdir(addr), State: map[string]any{"inputs": c.Inputs, "outputs": c.Outputs}},
		Stdout: out, Stderr: errw}
	if d.Plugin != "" {
		// down of a plugin runs from its directory with the inputs it was
		// written with.
		call.PluginDir, call.Inputs = filepath.Join(e.p.Dir, filepath.FromSlash(d.Plugin)), c.Inputs
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
		return fmt.Errorf("down exited with %d", res.ExitCode)
	}
	return nil
}

// downCommand reads the program of a down contract: a script, or argv.
func downCommand(run any) (model.Command, error) {
	switch x := run.(type) {
	case string:
		return model.Command{Script: x}, nil
	case []any:
		var argv []string
		for _, a := range x {
			s, ok := a.(string)
			if !ok {
				return model.Command{}, fmt.Errorf("the record's down program has %v, not a string", a)
			}
			argv = append(argv, s)
		}
		return model.Command{Argv: argv}, nil
	}
	return model.Command{}, fmt.Errorf("the record's down program is %T, not a script or a list", run)
}
