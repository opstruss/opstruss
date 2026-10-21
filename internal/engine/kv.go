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
	e         *engine
	mu        sync.Mutex
	read      map[string]any
	sensitive map[string]bool // the names, and prefixes with *, read that hold sensitive values

	once   sync.Once
	locals map[string]any
}

func (e *engine) newKV() *kvMemory {
	return &kvMemory{e: e, read: map[string]any{}, sensitive: map[string]bool{}}
}

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
	if prefix, ok := strings.CutSuffix(key, "*"); ok {
		m, sensitive, err := k.e.store.KVList(prefix, time.Now())
		if err != nil {
			return nil, err
		}
		for name := range sensitive {
			k.e.known("kv "+name, m[name])
			k.sensitive[key] = true
		}
		v = m
	} else {
		var sensitive bool
		var err error
		if v, sensitive, _, err = k.e.store.KVGet(key, time.Now()); err != nil {
			return nil, err
		}
		if sensitive {
			k.e.known("kv "+key, v)
			k.sensitive[key] = true
		}
	}
	k.read[key] = v
	return v, nil
}

// marked says whether kv() of key read a sensitive value: a name, a prefix
// with *, or for "", a name known only when it ran, whichever it was.
func (k *kvMemory) marked(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key != "" {
		return k.sensitive[key]
	}
	return len(k.sensitive) > 0
}

// scope gives s this memory: kv() reads through it, and the locals of the
// project that read kv() are evaluated with it.
func (k *kvMemory) scope(s *expr.Scope) *expr.Scope {
	s = s.WithKV(k.get, k.marked)
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
// The marks of the inputs say whether value is sensitive, and so kept
// encrypted, as is a value that holds a sensitive value the run knows; the
// marks of the outputs say whether value is.
func (e *engine) kvCall(ctx context.Context, in, marks map[string]any, write, expect bool) (*builtin.Result, map[string]any) {
	fail := func(err error) (*builtin.Result, map[string]any) {
		return &builtin.Result{Outputs: map[string]any{}, Err: err}, nil
	}
	name, _ := in["key"].(string)
	if !write {
		v, sensitive, _, err := e.store.KVGet(name, time.Now())
		if err != nil {
			return fail(err)
		}
		if sensitive {
			e.known("kv "+name, v)
			return &builtin.Result{Outputs: map[string]any{"value": v}}, map[string]any{"value": true}
		}
		return &builtin.Result{Outputs: map[string]any{"value": v}}, nil
	}
	sensitive := marks["value"] != nil
	if !sensitive && e.redact.Contains(in["value"]) {
		sensitive = true
		e.warn("", "kv %s gets a value that holds a sensitive value, so it is kept encrypted", name)
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
	cur, curSensitive, etag, err := e.store.KVGet(name, now)
	if err != nil {
		return fail(err)
	}
	if curSensitive {
		e.known("kv "+name, cur)
	}
	if expect && expr.JSON(cur) != expr.JSON(in["expect"]) {
		res := &builtin.Result{Outputs: map[string]any{"value": cur},
			Err: fmt.Errorf("%s holds %s, not %s that expect says", name, expr.JSON(cur), expr.JSON(in["expect"]))}
		if curSensitive {
			return res, map[string]any{"value": true}
		}
		return res, nil
	}
	if err := e.store.KVPut(name, in["value"], sensitive, ttl, etag, now); err != nil {
		return fail(err)
	}
	res := &builtin.Result{Outputs: map[string]any{"value": in["value"]}, Changed: expr.JSON(cur) != expr.JSON(in["value"]) || curSensitive != sensitive}
	if sensitive {
		return res, map[string]any{"value": true}
	}
	return res, nil
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
