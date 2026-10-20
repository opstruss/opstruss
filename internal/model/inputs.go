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
// the stack's with, the input's env, then its default.
func (p *Project) ResolveInputs(given map[string]string, lookupEnv func(string) (string, bool)) (map[string]InputValue, error) {
	for _, name := range slices.Sorted(maps.Keys(given)) {
		if p.Inputs[name] == nil {
			return nil, fmt.Errorf("--input %s: the project has no input %s", name, name)
		}
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
		} else if text, ok := lookup(lookupEnv, in.Env); ok {
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
