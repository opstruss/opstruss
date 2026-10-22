package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/record"
)

// A run that a watch makes carries at most MaxItems lines or paths, and
// MaxBytes of them; the rest are only counted.
const (
	MaxItems = 10000
	MaxBytes = 1 << 20
	// MaxDepth is how long a chain of after triggers grows.
	MaxDepth = 3
)

// watch makes runs of the events of the watch tr: the lines of a service,
// the lines of a program that it runs as a source, or changes of files.
// if: chooses each event, and batch or debounce gathers them into one run.
// It calls armed once its source sees events, or once it gives up.
func (t *triggers) watch(ctx context.Context, tr *engine.Trigger, armed func()) {
	ready := sync.OnceFunc(armed)
	defer ready()
	opts := t.options()
	f, err := engine.NewFilter(opts, tr)
	if err != nil {
		t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
		return
	}
	events := make(chan map[string]any, 1024)
	dropped := 0
	push := func(ev map[string]any) {
		// A source is never held up: an event that finds no room is dropped
		// and counted.
		select {
		case events <- ev:
		default:
			dropped++
			if dropped == 1 || dropped%1000 == 0 {
				t.d.event("diagnostic", "warn", tr.Task, "%s: dropped %d events that came faster than runs take them", tr, dropped)
			}
		}
	}
	switch {
	case tr.Unit != "" && opts.Model.Units[tr.Unit] != nil && opts.Model.Units[tr.Unit].Service != nil:
		stop := t.d.sup.listen(opts.Model.StackName+"/"+tr.Unit, func(stream, line string) {
			if tr.Stream == "both" || tr.Stream == stream {
				push(map[string]any{"unit": tr.Unit, "stream": stream, "line": line})
			}
		})
		defer stop()
	case len(tr.Paths) > 0:
		go t.paths(ctx, tr, push, ready)
	default:
		if err := t.source(ctx, tr, push); err != nil {
			t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
			return
		}
	}
	if len(tr.Paths) == 0 {
		ready()
	}
	gather := tr.Batch + tr.Debounce
	var batch []map[string]any
	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			ok, err := f.Match(ev)
			if err != nil {
				t.d.event("trigger", "warn", tr.Task, "%s: if: %v", tr, err)
				continue
			}
			if !ok {
				continue
			}
			if gather == 0 {
				t.fireEvent(tr, ev, 0)
				continue
			}
			batch = append(batch, ev)
			if timer == nil || tr.Debounce > 0 {
				timer = time.After(gather)
			}
		case <-timer:
			t.fireEvent(tr, gathered(tr, batch), 0)
			batch, timer = nil, nil
		}
	}
}

// gathered is the event of a run that a batch or debounce makes of evs. A
// file that changed more than once in them is there once, with what
// happened to it first.
func gathered(tr *engine.Trigger, evs []map[string]any) map[string]any {
	key, items := "lines", []any{}
	if len(tr.Paths) > 0 {
		key = "paths"
		seen := map[any]bool{}
		evs = slices.DeleteFunc(slices.Clone(evs), func(ev map[string]any) bool {
			p := ev["paths"].([]any)[0].(map[string]any)["path"]
			dup := seen[p]
			seen[p] = true
			return dup
		})
	}
	out := map[string]any{"count": int64(len(evs))}
	size := 0
	for _, ev := range evs {
		item := ev["line"]
		if key == "paths" {
			item = ev["paths"].([]any)[0]
		}
		if len(items) >= MaxItems || size > MaxBytes {
			break
		}
		data, _ := json.Marshal(item)
		size += len(data)
		items = append(items, item)
	}
	out[key] = items
	if tr.Unit != "" {
		out["unit"], out["stream"] = tr.Unit, tr.Stream
	}
	return out
}

// source runs the program of a watch as the daemon runs a service: in a
// session of its own, started again with backoff when it exits, with its
// inputs read again each time; each line it writes is an event.
func (t *triggers) source(ctx context.Context, tr *engine.Trigger, push func(map[string]any)) error {
	opts := t.options()
	addr := opts.Model.StackName + "/_watch/" + tr.Task + "/" + tr.Key
	dir := filepath.Join(t.d.root, "daemon", opts.Model.StackName, "_watch", tr.Task, tr.Key)
	prog, err := engine.WatchSource(opts, tr, dir)
	if err != nil {
		return err
	}
	spec := &engine.Service{Address: addr, Dir: dir, Program: prog, Restart: "always", Stop: plugin.ServiceStop,
		Rebuild: func() (*plugin.Program, error) { return engine.WatchSource(opts, tr, dir) },
		Lines: func(stream, line string) {
			ev := map[string]any{"line": line}
			if tr.Unit != "" {
				ev["unit"], ev["stream"] = tr.Unit, stream
			} else if stream != "stdout" {
				return
			}
			var data any
			if json.Unmarshal([]byte(line), &data) == nil {
				ev["data"] = data
			}
			push(ev)
		}}
	if err := t.d.sup.Start(ctx, spec); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = t.d.sup.Stop(context.Background(), addr)
	}()
	return nil
}

// fireEvent makes a run of tr for an event with fields, unless its task has
// MaxPending runs unfinished, and lets the after triggers know how it
// ended; depth is how deep in a chain of after triggers it is.
func (t *triggers) fireEvent(tr *engine.Trigger, fields map[string]any, depth int) {
	if !t.begin(tr.Task) {
		t.d.event("diagnostic", "warn", tr.Task, "%s: dropped an event, since task %s has %d runs unfinished", tr, tr.Task, MaxPending)
		return
	}
	opts := t.options()
	t.runs.Add(1)
	go func() {
		defer t.runs.Done()
		defer t.end(tr.Task)
		at := time.Now().UTC()
		opts.Event = &engine.TriggerEvent{Trigger: tr.Kind, Time: at, Fields: fields}
		opts.Supervisor = t.d.sup
		res, err := engine.Fire(t.d.runs, opts, tr)
		t.report(tr, at, res, err)
		if res != nil {
			t.ended(res.Run, depth)
		}
	}()
}

// ended makes the runs of the after triggers that the run that ended
// matches; depth is how deep in a chain that run is.
func (t *triggers) ended(run string, depth int) {
	rec, err := record.Read(filepath.Join(t.d.root, "runs", t.d.opts.Stack, run))
	if err != nil {
		return
	}
	name := rec.Task
	if name == "" {
		name = rec.Command
	}
	t.mu.Lock()
	afters := t.afters
	t.mu.Unlock()
	for _, tr := range afters {
		if tr.After != name || (tr.Unit != "" && rec.Units[tr.Unit] == nil) || (tr.Outcome != "any" && tr.Outcome != rec.Outcome) {
			continue
		}
		if depth+1 > MaxDepth {
			t.d.event("trigger", "warn", tr.Task, "%s: run %s is %d deep in a chain of after, so it makes no run", tr, run, depth)
			continue
		}
		outputs := rec.Outputs
		if outputs == nil {
			outputs = map[string]any{}
		}
		t.fireEvent(tr, map[string]any{"task": name, "unit": tr.Unit, "outcome": rec.Outcome, "outputs": outputs}, depth+1)
	}
}

// pathOf says whether the file at abs is one of the paths of the watch tr,
// a relative pattern read from the directory of its task and an absolute
// one as it is, where the last pattern that matches decides and an
// exclude: takes it out; and it gives the path that event.paths shows:
// relative to that directory for a file under it, absolute otherwise.
func pathOf(tr *engine.Trigger, abs string) (string, bool) {
	rel, err := filepath.Rel(tr.Dir, abs)
	inside := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	shown := filepath.ToSlash(abs)
	if inside {
		shown = filepath.ToSlash(rel)
	}
	in := false
	for _, it := range tr.Paths {
		target := filepath.ToSlash(abs)
		if !filepath.IsAbs(it.Pattern) {
			if !inside {
				continue
			}
			target = shown
		}
		if ok, _ := doublestar.Match(filepath.ToSlash(it.Pattern), target); ok {
			in = !it.Exclude
		}
	}
	return shown, in
}

// pathRoots are the directories that the watch tr looks under: that of its
// task, and the fixed part of each absolute pattern outside it.
func pathRoots(tr *engine.Trigger) []string {
	roots := []string{tr.Dir}
	for _, it := range tr.Paths {
		if it.Exclude || !filepath.IsAbs(it.Pattern) {
			continue
		}
		base, _ := doublestar.SplitPattern(filepath.ToSlash(it.Pattern))
		base = filepath.FromSlash(base)
		if rel, err := filepath.Rel(tr.Dir, base); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if !slices.Contains(roots, base) {
			roots = append(roots, base)
		}
	}
	return roots
}
