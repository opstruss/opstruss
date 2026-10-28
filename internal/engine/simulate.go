package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// MaxEvent is the largest event that --event takes: it holds a webhook
// event with a body of 1 MiB, also when JSON escapes much of it.
const MaxEvent = 4 << 20

// eventFields are the fields of an event of each trigger beside trigger
// and time, as a caller of --event writes it: one event as its source
// gives it, not what a batch or debounce gathers of many.
var eventFields = map[string][]string{
	"schedule": nil,
	"every":    nil,
	"webhook":  {"method", "path", "headers", "query", "body"},
	"watch":    {"unit", "stream", "line", "paths"},
	"after":    {"task", "unit", "outcome", "outputs"},
}

// fieldTypes are what the fields of eventFields hold; body holds anything.
var fieldTypes = map[string]string{
	"method": "a string", "path": "a string", "unit": "a string", "stream": "a string", "line": "a string",
	"task": "a string", "outcome": "a string", "headers": "an object of strings", "query": "an object of strings",
	"outputs": "an object", "paths": "a list",
}

// Simulate runs the task name for ev, an event that its caller wrote, as
// a trigger of the task runs it for an event of its source: the trigger
// of the kind that ev.trigger names which ev fits, the filter of a watch
// or if: of another trigger, the batch or debounce of a watch gathering
// that one event without waiting, then with:, and over it inputs and the
// flags of opts, checked against the inputs of the task before the run
// starts. The run is of the command line, and its record says which
// trigger it simulated. It returns nil, and makes no run, when the
// trigger leaves the event out.
func Simulate(ctx context.Context, opts Options, name string, inputs, ev map[string]any) (*Result, error) {
	e, err := loadContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	e.ctx, e.trigger = ctx, true
	tr, at, fields, err := simulated(e.p, name, ev)
	if err != nil {
		return nil, fmt.Errorf("--event: %w", err)
	}
	if at.IsZero() {
		at = e.started
	}
	left := func(why string) (*Result, error) {
		e.tell(name, "%s: %s, so there is no run", tr, why)
		return nil, nil
	}
	switch tr.Kind {
	case "watch":
		f, err := NewFilter(opts, tr)
		if err != nil {
			return nil, err
		}
		ok, err := f.Match(fields, at)
		if err != nil {
			return nil, fmt.Errorf("if: %w", err)
		}
		if !ok {
			return left("if: leaves the event out")
		}
		if tr.Batch+tr.Debounce > 0 {
			b := NewBatch(tr)
			b.Add(fields)
			fields = b.Event()
		}
	case "after":
		if tr.Outcome != "any" && fields["outcome"] != tr.Outcome {
			return left(fmt.Sprintf("it takes runs that end %s, not %s", tr.Outcome, fields["outcome"]))
		}
	}
	flags, err := readFlags(e.p.Tasks[name].Inputs, e.opts.Flags)
	if err != nil {
		return nil, err
	}
	over := maps.Clone(inputs)
	if over == nil && len(flags) > 0 {
		over = map[string]any{}
	}
	maps.Copy(over, flags)
	e.opts.Flags = nil // they are in over now
	e.opts.Event = &TriggerEvent{Trigger: tr.Kind, Time: at, Fields: fields, Simulated: true}
	e.event = eventOf(e.opts.Event, e.started)
	res, err := e.fire(ctx, tr, over, true)
	if err == nil && res == nil {
		return left("if: leaves the event out")
	}
	return res, err
}

// tell says something of a call that makes no run to its caller, as no
// record keeps it.
func (e *engine) tell(task, format string, args ...any) {
	if e.opts.Emit != nil {
		e.opts.Emit(Event{V: record.EventVersion, Time: record.Stamp(time.Now()), Type: "task", Level: "info",
			Task: task, Message: e.redact.Log(fmt.Sprintf(format, args...))})
	}
}

// simulated reads ev, an event that the caller of the task name wrote,
// as the trigger that it fits would get it from its source: its fields
// checked and completed as the daemon gives them, and its time, which is
// zero when ev has none.
func simulated(p *model.Project, name string, ev map[string]any) (*Trigger, time.Time, map[string]any, error) {
	var at time.Time
	kind, _ := ev["trigger"].(string)
	allowed, ok := eventFields[kind]
	if !ok {
		return nil, at, nil, fmt.Errorf(`trigger is schedule, every, webhook, watch, or after, as in {"trigger": "webhook"}`)
	}
	for _, k := range slices.Sorted(maps.Keys(ev)) {
		v := ev[k]
		switch {
		case k == "trigger":
		case k == "time":
			s, _ := v.(string)
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, at, nil, fmt.Errorf("time is a time as RFC 3339 writes it, such as 2026-10-23T09:00:00Z")
			}
			at = t.UTC()
		case slices.Contains([]string{"lines", "count", "truncated", "data"}, k):
			return nil, at, nil, fmt.Errorf("%s is what the daemon makes of the events of a watch; write one event as its source gives it: line, or paths with one change", k)
		case !slices.Contains(allowed, k):
			return nil, at, nil, fmt.Errorf("an event of %s has no field %s; it has %s", kind, k, strings.Join(append([]string{"trigger", "time"}, allowed...), ", "))
		case !holds(v, fieldTypes[k]):
			return nil, at, nil, fmt.Errorf("%s is %s", k, fieldTypes[k])
		}
	}
	if t := p.Tasks[name]; t == nil || t.On == nil {
		return nil, at, nil, fmt.Errorf("task %s has no triggers", name)
	}
	all, err := triggersOf(p, name)
	if err != nil {
		return nil, at, nil, err
	}
	var have, fit []*Trigger
	for _, t := range all {
		if t.Kind == kind {
			have = append(have, t)
			if fits(t, ev) {
				fit = append(fit, t)
			}
		}
	}
	names := func(ts []*Trigger) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.String())
		}
		return strings.Join(out, "; ")
	}
	switch {
	case len(have) == 0:
		return nil, at, nil, fmt.Errorf("task %s has no %s trigger", name, kind)
	case len(fit) == 0:
		return nil, at, nil, fmt.Errorf("the event fits no %s trigger of task %s, which has %s", kind, name, names(have))
	case len(fit) > 1:
		return nil, at, nil, fmt.Errorf("the event fits %d triggers of task %s: %s; %s", len(fit), name, names(fit), map[string]string{
			"webhook": "write its method and path", "watch": "write its unit, or paths for a watch of files",
			"after": "write its task and unit"}[kind]+"; triggers of one schedule or interval cannot be told apart")
	}
	tr := fit[0]
	fields, err := completed(p, tr, ev)
	return tr, at, fields, err
}

// holds reports whether v is what fieldTypes says.
func holds(v any, typ string) bool {
	switch typ {
	case "a string":
		_, ok := v.(string)
		return ok
	case "an object", "an object of strings":
		m, ok := v.(map[string]any)
		for _, x := range m {
			if _, s := x.(string); !s && typ == "an object of strings" {
				return false
			}
		}
		return ok
	case "a list":
		_, ok := v.([]any)
		return ok
	}
	return true
}

// fits reports whether ev, an event of the kind of t, says nothing that
// tells it from t.
func fits(t *Trigger, ev map[string]any) bool {
	is := func(k, v string) bool {
		s, ok := ev[k].(string)
		return !ok || s == v
	}
	switch t.Kind {
	case "webhook":
		m, ok := ev["method"].(string)
		return (!ok || strings.EqualFold(m, t.Method)) && is("path", t.Path)
	case "watch":
		_, paths := ev["paths"]
		return paths == (len(t.Paths) > 0) && is("unit", t.Unit)
	case "after":
		return is("task", t.After) && is("unit", t.Unit)
	}
	return true
}

// completed are the fields of ev as the source of tr gives them: what tr
// says filled in, the names of headers in lower case, and data parsed from
// a line of a program.
func completed(p *model.Project, tr *Trigger, ev map[string]any) (map[string]any, error) {
	switch tr.Kind {
	case "webhook":
		headers := map[string]any{}
		if h, ok := ev["headers"].(map[string]any); ok {
			for k, v := range h {
				headers[strings.ToLower(k)] = v
			}
		}
		query, _ := ev["query"].(map[string]any)
		if query == nil {
			query = map[string]any{}
		}
		body, ok := ev["body"]
		if !ok {
			body = ""
		}
		return map[string]any{"method": tr.Method, "path": tr.Path, "headers": headers, "query": query, "body": body}, nil
	case "after":
		outcome, _ := ev["outcome"].(string)
		outcome = cmp.Or(outcome, "success")
		if !slices.Contains([]string{"success", "failure", "skipped", "cancelled"}, outcome) {
			return nil, fmt.Errorf("outcome is success, failure, skipped, or cancelled, not %q", outcome)
		}
		outputs, _ := ev["outputs"].(map[string]any)
		if outputs == nil {
			outputs = map[string]any{}
		}
		return map[string]any{"task": tr.After, "unit": tr.Unit, "outcome": outcome, "outputs": outputs}, nil
	case "watch":
		return watchEvent(p, tr, ev)
	}
	return map[string]any{}, nil
}

// watchEvent is ev as the source of the watch tr gives one event: a change
// of a file, or a line.
func watchEvent(p *model.Project, tr *Trigger, ev map[string]any) (map[string]any, error) {
	kind := WatchKind(p, tr)
	if kind == "paths" {
		for _, k := range []string{"line", "stream", "unit"} {
			if _, ok := ev[k]; ok {
				return nil, fmt.Errorf("a watch of paths has no %s", k)
			}
		}
		list := ev["paths"].([]any)
		change, _ := list[0].(map[string]any)
		path, _ := change["path"].(string)
		op, _ := change["op"].(string)
		if len(list) != 1 || len(change) != 2 || path == "" || !slices.Contains([]string{"create", "write", "remove", "rename"}, op) {
			return nil, fmt.Errorf(`paths holds one change of a file, as its source gives it, such as [{"path": "notes/a.md", "op": "write"}], where op is create, write, remove, or rename`)
		}
		return map[string]any{"paths": []any{map[string]any{"path": path, "op": op}}, "count": int64(1)}, nil
	}
	line, ok := ev["line"].(string)
	if !ok {
		return nil, fmt.Errorf("a watch of lines needs line, the line its source writes")
	}
	out := map[string]any{"line": line}
	stream, given := ev["stream"].(string)
	switch {
	case tr.Unit == "" && given && stream != "stdout":
		return nil, fmt.Errorf("a watch of a call hears what it writes to stdout, not %s", stream)
	case tr.Unit != "":
		if !given {
			stream = map[bool]string{true: "stdout", false: tr.Stream}[tr.Stream == "both"]
		}
		if stream != "stdout" && stream != "stderr" || tr.Stream != "both" && stream != tr.Stream {
			return nil, fmt.Errorf("%s hears %s, not %s", tr, map[bool]string{true: "stdout and stderr", false: tr.Stream}[tr.Stream == "both"], stream)
		}
		out["unit"], out["stream"] = tr.Unit, stream
	}
	if kind == "program" {
		var data any
		if json.Unmarshal([]byte(line), &data) == nil {
			out["data"] = data
		}
	}
	return out, nil
}
