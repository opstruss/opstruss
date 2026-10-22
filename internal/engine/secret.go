package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/secret"
)

// secretValues is secrets.<name> of the run: each source is resolved the
// first time an expression reads it, once a run, so a secret that nothing
// running reads is never read, and one that fails fails only what reads
// it, with its name.
func (e *engine) secretValues() map[string]any {
	out := make(map[string]any, len(e.p.Secrets))
	for name := range e.p.Secrets {
		out[name] = expr.Later(func() (any, error) {
			v, err := e.resolveSecret(name)
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", name, err)
			}
			e.known("secret "+name, v)
			return v, nil
		})
	}
	return out
}

// resolveSecret reads secret name from its source, whose expressions read
// what is known at plan: the stack, the inputs and locals of the project,
// and the facts. Outside a run only the expressions of a trigger read
// secrets, and only from sops and env, because a program runs in a run.
func (e *engine) resolveSecret(name string) (any, error) {
	if e.run == nil && !e.trigger {
		return nil, errors.New("secrets are read only in a run")
	}
	src, dir := e.p.Secrets[name], e.p.DirOf("secrets."+name)
	s := e.newKV().scope(&expr.Scope{Dir: dir, Vars: map[string]any{"stack": e.p.StackName, "inputs": e.inputs,
		"locals": e.locals, "opstruss": e.facts(nil)}, Marks: map[string]any{"inputs": e.inputMarks, "locals": e.localMarks}})
	e.debug("", "resolving secret %s", name)
	switch {
	case src.Sops != "":
		return e.sopsSecret(src, s)
	case src.EnvName != "":
		v, ok := e.opts.LookupEnv(src.EnvName)
		if !ok {
			return nil, fmt.Errorf("%s is not set", src.EnvName)
		}
		return v, nil
	case e.run == nil:
		return nil, errors.New("it comes from a program, which runs only in a run; a trigger reads secrets from sops and env")
	case src.Run != nil:
		// What the program prints is the value, so it stays out of the log.
		r := e.program(e.ctx, progSpec{label: "secrets." + name, cmd: src.Run, shell: src.Shell, dir: src.Dir, base: dir,
			env: src.Env, pass: src.Pass, outputs: map[string]*model.Output{"value": {From: "stdout"}}, secret: true}, s)
		if r.outcome != success {
			return nil, errors.New("its program did not succeed")
		}
		return r.outputs["value"], nil
	}
	for callee, args := range src.Calls {
		r := e.callByName(e.ctx, progSpec{label: "secrets." + name, base: dir, env: src.Env, pass: src.Pass, secret: true},
			callee, args, s)
		switch {
		case r.outcome != success:
			return nil, fmt.Errorf("%s did not succeed", callee)
		case r.outputs["value"] == nil:
			return nil, fmt.Errorf("%s gave no value", callee)
		}
		return r.outputs["value"], nil
	}
	return nil, errors.New("it has no source")
}

// sopsSecret reads a sops file, each one once a run: the whole of it, or
// the value at field, a path of keys such as postgres.password.
func (e *engine) sopsSecret(src *model.Secret, s *expr.Scope) (any, error) {
	file, err := expr.EvalText(src.Sops, s)
	if err != nil {
		return nil, fmt.Errorf("sops: %w", err)
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Dir, path)
	}
	e.sopsMu.Lock()
	v, ok := e.sopsFiles[path]
	if !ok {
		v = expr.Later(func() (any, error) {
			sops := &secret.Sops{Dir: e.p.Dir, Environ: e.opts.Environ}
			data, tree, err := sops.Decrypt(path, file)
			if err != nil {
				return nil, err
			}
			if tree {
				return plugin.Decode(data, "json")
			}
			return plugin.Decode(data, "text")
		})
		e.sopsFiles[path] = v
	}
	e.sopsMu.Unlock()
	whole, err := expr.Made(v)
	if err != nil || src.Field == "" {
		return whole, err
	}
	field, err := expr.EvalText(src.Field, s)
	if err != nil {
		return nil, fmt.Errorf("field: %w", err)
	}
	v = whole
	for _, k := range strings.Split(field, ".") {
		switch x := v.(type) {
		case map[string]any:
			v, ok = x[k]
		case []any:
			i, err := strconv.Atoi(k)
			ok = err == nil && i >= 0 && i < len(x)
			if ok {
				v = x[i]
			}
		default:
			ok = false
		}
		if !ok {
			return nil, fmt.Errorf("%s has no %s", file, field)
		}
	}
	return v, nil
}
