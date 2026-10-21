package model

import (
	"fmt"
	"maps"
	"slices"
)

// InputValue is the value of a project input and where it came from:
// --input, stack, env, or default.
type InputValue struct {
	Value any    `json:"value"`
	From  string `json:"from"`
}

// ResolveInputs gives each project input its value, in the order --input,
// the stack's with, the input's env, then its default. known, when not
// nil, gets the value of each sensitive input before it is checked, so the
// caller can hide it also in the error the check gives.
func (p *Project) ResolveInputs(given map[string]string, lookupEnv func(string) (string, bool), known func(name string, v any)) (map[string]InputValue, error) {
	for _, name := range slices.Sorted(maps.Keys(given)) {
		switch in := p.Inputs[name]; {
		case in == nil:
			return nil, fmt.Errorf("--input %s: the project has no input %s", name, name)
		case in.Sensitive:
			return nil, fmt.Errorf("--input %s: input %s is sensitive, so it is not taken from the command line, which shell history and the list of processes keep; give it through env: or the stack", name, name)
		}
	}
	if known == nil {
		known = func(string, any) {}
	}
	out := map[string]InputValue{}
	for _, name := range slices.Sorted(maps.Keys(p.Inputs)) {
		in := p.Inputs[name]
		var v any
		var from string
		if text, ok := given[name]; ok {
			parsed, err := ParseText(text, in.Type)
			if err != nil {
				return nil, fmt.Errorf("--input %s: %w", name, err)
			}
			v, from = parsed, "--input"
		} else if sv, ok := p.Stack.With[name]; ok {
			v, from = sv, "stack"
			if in.Sensitive {
				known(name, sv)
			}
		} else if text, ok := lookup(lookupEnv, in.Env); ok {
			if in.Sensitive {
				known(name, text)
			}
			parsed, err := ParseText(text, in.Type)
			if err != nil {
				return nil, fmt.Errorf("input %s: %s: %w", name, in.Env, err)
			}
			v, from = parsed, "env"
		} else if in.Default != nil {
			from = "default"
		}
		c, err := in.Conform(v)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		if c != nil {
			out[name] = InputValue{Value: c, From: from}
		}
	}
	return out, nil
}

func lookup(lookupEnv func(string) (string, bool), name string) (string, bool) {
	if name == "" || lookupEnv == nil {
		return "", false
	}
	return lookupEnv(name)
}

// HasSensitive reports whether the project has a secret, or declares a
// sensitive input or output: of itself, its tasks and units, the
// components they use, and its plugins. A stack of such a project needs
// its data key.
func (p *Project) HasSensitive() bool {
	return len(p.Secrets) > 0 || p.declaresSensitive(map[*Project]bool{})
}

func (p *Project) declaresSensitive(seen map[*Project]bool) bool {
	if seen[p] {
		return false
	}
	seen[p] = true
	if slices.ContainsFunc(slices.Collect(maps.Values(p.Outputs)), func(o *ProjectOutput) bool { return o.Sensitive }) ||
		sensitiveIO(p.Inputs, nil) {
		return true
	}
	tasks := slices.Collect(maps.Values(p.Tasks))
	for _, u := range p.Units {
		if sensitiveIO(nil, u.Outputs) {
			return true
		}
		tasks = append(tasks, slices.Collect(maps.Values(u.Tasks))...)
		if t := u.Target; t != nil && t.Component != nil && t.Component.declaresSensitive(seen) {
			return true
		}
	}
	for _, pkg := range p.Packages {
		if pl := pkg.Plugin; pl != nil {
			if sensitiveIO(pl.Inputs, pl.Outputs.Declared) {
				return true
			}
			tasks = append(tasks, slices.Collect(maps.Values(pl.Tasks))...)
		}
	}
	return slices.ContainsFunc(tasks, func(t *Task) bool { return sensitiveIO(t.Inputs, t.Outputs) })
}

func sensitiveIO(inputs map[string]*Input, outputs map[string]*Output) bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(inputs)), func(in *Input) bool { return in.Sensitive }) ||
		slices.ContainsFunc(slices.Collect(maps.Values(outputs)), func(o *Output) bool { return o.Sensitive })
}
