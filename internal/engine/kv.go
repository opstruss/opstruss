package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/builtin"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/state"
)

// kvMemory is what kv() reads in one plan or up of units, or in one
// evaluation of a task or step: each name once, so every read in it sees
// the same value, and the locals that read kv() evaluated with it.
type kvMemory struct {
	e    *engine
	mu   sync.Mutex
	read map[string]any

	once   sync.Once
	locals map[string]any
}

func (e *engine) newKV() *kvMemory { return &kvMemory{e: e, read: map[string]any{}} }

// passKV is the memory that the units of the current pass read; outside a
// pass, as for the outputs of the project, one for this run.
func (e *engine) passKV() *kvMemory {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kv == nil {
		e.kv = e.newKV()
	}
	return e.kv
}

// get reads a name, or every name with a prefix when key ends in *.
func (k *kvMemory) get(key string) (any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.read[key]; ok {
		return v, nil
	}
	var v any
	var err error
	if prefix, ok := strings.CutSuffix(key, "*"); ok {
		var m map[string]any
		m, err = k.e.store.KVList(prefix, time.Now())
		v = m
	} else {
		v, _, err = k.e.store.KVGet(key, time.Now())
	}
	if err != nil {
		return nil, err
	}
	k.read[key] = v
	return v, nil
}

// scope gives s this memory: kv() reads through it, and the locals of the
// project that read kv() are evaluated with it.
func (k *kvMemory) scope(s *expr.Scope) *expr.Scope {
	s = s.WithKV(k.get)
	if !k.e.kvLocals {
		return s
	}
	return s.With("locals", k.localsOf(k.e.top, k.e.inputs))
}

// localsOf is locals.<name> in f with this memory: those of the project,
// with the ones that read kv() evaluated with it once, or those of the
// component, evaluated with inputs, its inputs as they are now.
func (k *kvMemory) localsOf(f *frame, inputs map[string]any) map[string]any {
	if f.node != nil {
		return f.in.ComponentLocals(k.e.p.Name, inputs, k.get)
	}
	if !k.e.kvLocals {
		return k.e.locals
	}
	k.once.Do(func() {
		var err error
		if k.locals, err = k.e.p.EvalLocals(k.e.inputs, k.get); err != nil {
			k.locals = k.e.locals
		}
	})
	return k.locals
}

// kvCall reads a name of the memory of the stack, or with write sets it to
// value, and removes it for null, under the lock of the memory; with
// expect, only while it still holds what expect says, and otherwise the
// call fails with the value it holds, for retry to try again with.
func (e *engine) kvCall(ctx context.Context, in map[string]any, write, expect bool) *builtin.Result {
	fail := func(err error) *builtin.Result { return &builtin.Result{Outputs: map[string]any{}, Err: err} }
	name, _ := in["key"].(string)
	if !write {
		v, _, err := e.store.KVGet(name, time.Now())
		if err != nil {
			return fail(err)
		}
		return &builtin.Result{Outputs: map[string]any{"value": v}}
	}
	var ttl time.Duration
	if t, ok := in["ttl"].(string); ok {
		var err error
		if ttl, err = time.ParseDuration(t); err != nil {
			return fail(fmt.Errorf("ttl: %w", err))
		}
	}
	if err := state.CheckKVName(name); err != nil {
		return fail(err)
	}
	token, err := e.kvLock(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = e.store.KVUnlock(token) }()
	now := time.Now()
	cur, etag, err := e.store.KVGet(name, now)
	if err != nil {
		return fail(err)
	}
	if expect && expr.JSON(cur) != expr.JSON(in["expect"]) {
		return &builtin.Result{Outputs: map[string]any{"value": cur},
			Err: fmt.Errorf("%s holds %s, not %s that expect says", name, expr.JSON(cur), expr.JSON(in["expect"]))}
	}
	if err := e.store.KVPut(name, in["value"], ttl, etag, now); err != nil {
		return fail(err)
	}
	return &builtin.Result{Outputs: map[string]any{"value": in["value"]}, Changed: expr.JSON(cur) != expr.JSON(in["value"])}
}

// kvLock takes the lock of the memory of the stack, waiting while another
// run writes; writes hold it only for a moment.
func (e *engine) kvLock(ctx context.Context) (string, error) {
	for delay := 5 * time.Millisecond; ; delay = min(2*delay, 200*time.Millisecond) {
		token, err := e.store.KVLock(e.holder)
		var le *state.LockedError
		if !errors.As(err, &le) {
			return token, err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("waiting for the lock of kv: %w", context.Cause(ctx))
		case <-time.After(delay):
		}
	}
}
