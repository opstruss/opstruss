package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// decodeValue decodes a validated document into v. Numbers stay exact
// through the round trip and become int64 when whole.
func decodeValue(doc any, v any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := decodeJSON(b, v); err != nil {
		return err
	}
	fixNumbers(reflect.ValueOf(v))
	return nil
}

func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// fixNumbers replaces the json.Number values left in any-typed fields.
func fixNumbers(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			fixNumbers(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fixNumbers(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			fixNumbers(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() != reflect.Interface {
				fixNumbers(e)
				continue
			}
			if n := numbers(e.Interface()); n != nil {
				v.SetMapIndex(k, reflect.ValueOf(n))
			}
		}
	case reflect.Interface:
		if !v.IsNil() && v.CanSet() {
			v.Set(reflect.ValueOf(numbers(v.Interface())))
		}
	}
}

func numbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return wholeNumber(f)
	case map[string]any:
		for k, e := range x {
			x[k] = numbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbers(e)
		}
	}
	return v
}

// jsonKeys returns the keys that a struct type decodes.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// decodeCalls decodes the keys of b that are fields of v, a pointer to a
// struct, and returns the others as calls; x-* keys are ignored.
func decodeCalls(b []byte, v any) (Calls, error) {
	var raw map[string]json.RawMessage
	if err := decodeJSON(b, &raw); err != nil {
		return nil, err
	}
	known := jsonKeys(reflect.TypeOf(v).Elem())
	fields := map[string]json.RawMessage{}
	var calls Calls
	for k, m := range raw {
		switch {
		case known[k]:
			fields[k] = m
		case strings.HasPrefix(k, "x-"):
		default:
			var args map[string]any
			if err := decodeJSON(m, &args); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			if k == "assert" || k == "opstruss.assert" {
				assertConditions(args)
			}
			if calls == nil {
				calls = Calls{}
			}
			calls[k] = args
		}
	}
	fb, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return calls, decodeJSON(fb, v)
}

// assertConditions writes each condition of assert.that as the expression
// it is: that is a condition key, which, like if, may be written without
// ${{ }}.
func assertConditions(args map[string]any) {
	switch that := args["that"].(type) {
	case string:
		args["that"] = condition(that)
	case []any:
		for i, c := range that {
			if s, ok := c.(string); ok {
				that[i] = condition(s)
			}
		}
	}
}

func isString(b []byte) bool { return len(b) > 0 && b[0] == '"' }

func (c *Command) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &c.Script)
	}
	return json.Unmarshal(b, &c.Argv)
}

func (e *EnvValue) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &e.Text)
	}
	var f struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	e.Text, e.File = f.File, true
	return nil
}

func (p *PathItem) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &p.Pattern)
	}
	var f struct {
		Exclude string `json:"exclude"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	p.Pattern, p.Exclude = f.Exclude, true
	return nil
}

func (r *Requires) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &r.Expr)
	}
	return json.Unmarshal(b, &r.List)
}

func (u *Uses) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &u.Source)
	}
	type plain Uses
	return decodeJSON(b, (*plain)(u))
}

func (o *ProjectOutput) UnmarshalJSON(b []byte) error {
	if isString(b) {
		var s string
		err := json.Unmarshal(b, &s)
		o.Value = s
		return err
	}
	type plain ProjectOutput
	return decodeJSON(b, (*plain)(o))
}

func (k *EncryptionKey) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &k.Age)
	}
	return json.Unmarshal(b, &k.Sops)
}

func (k *EncryptionKey) MarshalJSON() ([]byte, error) {
	if k.Age != "" {
		return json.Marshal(k.Age)
	}
	return json.Marshal(k.Sops)
}

func (bk *Backend) UnmarshalJSON(b []byte) error {
	if isString(b) {
		bk.Local = true
		return nil
	}
	type plain Backend
	return decodeJSON(b, (*plain)(bk))
}

func (m *MCP) UnmarshalJSON(b []byte) error {
	if string(b) == "false" {
		m.Off = true
		return nil
	}
	type plain MCP
	return decodeJSON(b, (*plain)(m))
}

func (c *Concurrency) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &c.Mode)
	}
	type plain Concurrency
	return decodeJSON(b, (*plain)(c))
}

func (s *Schedule) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &s.Cron)
	}
	type plain Schedule
	return decodeJSON(b, (*plain)(s))
}

func (s *Signature) UnmarshalJSON(b []byte) error {
	if isString(b) {
		return json.Unmarshal(b, &s.Scheme)
	}
	type plain Signature
	return decodeJSON(b, (*plain)(s))
}

func (l *OneOrMany[T]) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		var list []T
		err := decodeJSON(b, &list)
		*l = list
		return err
	}
	var one T
	err := decodeJSON(b, &one)
	*l = []T{one}
	return err
}

func (o *PluginOutputs) UnmarshalJSON(b []byte) error {
	if isString(b) {
		o.Tool = true
		return nil
	}
	return decodeJSON(b, &o.Declared)
}

func (t *Task) UnmarshalJSON(b []byte) error {
	type plain Task
	calls, err := decodeCalls(b, (*plain)(t))
	t.Calls = calls
	return err
}

func (s *Step) UnmarshalJSON(b []byte) error {
	type plain Step
	calls, err := decodeCalls(b, (*plain)(s))
	s.Calls = calls
	return err
}

func (w *Watch) UnmarshalJSON(b []byte) error {
	type plain Watch
	calls, err := decodeCalls(b, (*plain)(w))
	w.Calls = calls
	return err
}

// UnmarshalJSON reads env as the name of a variable for an env source, and
// as variables for the program of a run source.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := decodeJSON(b, &raw); err != nil {
		return err
	}
	if env, ok := raw["env"]; ok {
		delete(raw, "env")
		if isString(env) {
			if err := json.Unmarshal(env, &s.EnvName); err != nil {
				return err
			}
		} else if err := decodeJSON(env, &s.Env); err != nil {
			return err
		}
	}
	rest, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	type plain Secret
	calls, err := decodeCalls(rest, (*plain)(s))
	s.Calls = calls
	return err
}
