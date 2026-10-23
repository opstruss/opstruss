package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// watchSpec is what an install prepared for a watch before it changes
// anything: the filter of its if:, and its source, the lines of a service,
// changes of files, or a program, with that program as it would start.
type watchSpec struct {
	kind   string // lines, paths, or program
	filter *engine.Filter
	addr   string // of the service whose lines it hears, or of its program
	dir    string // of its program: the files of its env: values
	prog   *plugin.Program
}

// prepareWatch prepares the watch b of the source id. The directory of
// its program is named by id, not by its place in the list, so a program
// that stays looks the same.
func (t *triggers) prepareWatch(id string, b *bound) (*watchSpec, error) {
	opts, tr := b.snap.opts, b.tr
	f, err := engine.NewFilter(opts, tr)
	if err != nil {
		return nil, err
	}
	spec := &watchSpec{filter: f}
	stack := opts.Model.StackName
	switch u := opts.Model.Units[tr.Unit]; {
	case tr.Unit != "" && u != nil && u.Service != nil:
		spec.kind, spec.addr = "lines", stack+"/"+tr.Unit
	case len(tr.Paths) > 0:
		spec.kind = "paths"
	default:
		name := id[strings.LastIndex(id, "/")+1:]
		spec.kind = "program"
		spec.addr = stack + "/_watch/" + tr.Task + "/" + name
		spec.dir = filepath.Join(t.d.root, "daemon", stack, "_watch", tr.Task, name)
		if spec.prog, err = engine.WatchSource(opts, tr, spec.dir); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

// watcher is the source of a watch and the loop that handles its events.
type watcher struct {
	t       *triggers
	s       *source
	kind    string
	events  chan map[string]any
	dropped atomic.Int64
	updates chan watchUpdate
	quit    chan struct{}

	mu      sync.Mutex
	prog    *plugin.Program // what its program runs now
	svc     *engine.Service
	closeFn func() // stops its source
	gone    bool   // its source gave up
}

// watchUpdate is a new snapshot for a watch; changed says that it differs
// from the one the watch has, so what the watch gathered goes first.
type watchUpdate struct {
	b       *bound
	f       *engine.Filter
	changed bool
	ack     chan struct{}
}

func newWatcher(t *triggers, s *source, spec *watchSpec) *watcher {
	return &watcher{t: t, s: s, kind: spec.kind, events: make(chan map[string]any, 1024), updates: make(chan watchUpdate),
		quit: make(chan struct{}), prog: spec.prog}
}

// same reports whether w is the watch that spec prepared: a source of the
// same kind that has not given up, and the same program.
func (w *watcher) same(spec *watchSpec) bool {
	w.mu.Lock()
	prog, svc, gone := w.prog, w.svc, w.gone
	w.mu.Unlock()
	switch {
	case gone || spec.kind != w.kind:
		return false
	case w.kind == "program":
		return svc != nil && w.t.d.sup.specLives(svc) && prog.Equal(spec.prog)
	}
	return true
}

// update hands w a new snapshot and waits until its loop took it.
func (w *watcher) update(b *bound, f *engine.Filter, changed bool) {
	ack := make(chan struct{})
	select {
	case w.updates <- watchUpdate{b: b, f: f, changed: changed, ack: ack}:
		select {
		case <-ack:
		case <-w.s.done:
		}
	case <-w.s.done:
	}
}

// close stops the source of w, which takes no event after that.
func (w *watcher) close() {
	w.mu.Lock()
	fn := w.closeFn
	w.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// drain makes the loop of w handle the events that its source took, run
// what it gathered, and end.
func (w *watcher) drain() {
	select {
	case w.quit <- struct{}{}:
	case <-w.s.done:
	}
}

// push takes an event from the source. A source is never held up: an
// event that finds no room is dropped and counted.
func (w *watcher) push(ev map[string]any) {
	select {
	case w.events <- ev:
	default:
		if n := w.dropped.Add(1); n == 1 || n%1000 == 0 {
			tr := w.s.cur.Load().tr
			w.t.d.event("diagnostic", "warn", tr.Task, "%s: dropped %d events that came faster than runs take them", tr, n)
		}
	}
}

// watch makes runs of the events of the watch of s: the lines of a
// service, the lines of a program that it runs as a source, or changes of
// files. if: chooses each event, and batch or debounce gathers them into
// one run. It calls ready once its source sees events, or with why it
// does not.
func (t *triggers) watch(ctx context.Context, s *source, spec *watchSpec, ready func(error)) {
	defer ready(nil)
	w, tr := s.w, s.cur.Load().tr
	switch spec.kind {
	case "lines":
		stop := t.d.sup.listen(spec.addr, func(stream, line string) {
			if tr.Stream == "both" || tr.Stream == stream {
				w.push(map[string]any{"unit": tr.Unit, "stream": stream, "line": line})
			}
		})
		w.mu.Lock()
		w.closeFn = stop
		w.mu.Unlock()
		ready(nil)
	case "paths":
		pctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		w.mu.Lock()
		w.closeFn = func() {
			stop()
			<-done
		}
		w.mu.Unlock()
		go func() {
			defer close(done)
			t.paths(pctx, tr, w.push, func() { ready(nil) })
			if pctx.Err() == nil {
				w.mu.Lock()
				w.gone = true
				w.mu.Unlock()
			}
		}()
	default:
		if err := t.source(ctx, w, spec); err != nil {
			t.forget(s)
			ready(err)
			return
		}
		ready(nil)
	}
	w.loop(ctx, spec.filter)
}

// loop handles the events of w with the snapshot that it has when it takes
// each: if: of that snapshot chooses it, and its batch or debounce gathers
// it. A new snapshot that differs runs what was gathered with the old one
// first; one that does not keeps it, and its time.
func (w *watcher) loop(ctx context.Context, f *engine.Filter) {
	cur := w.s.cur.Load()
	var batch []map[string]any
	var timer <-chan time.Time
	flush := func() {
		if len(batch) > 0 {
			w.t.fireEvent(cur, gathered(cur.tr, batch), 0)
		}
		batch, timer = nil, nil
	}
	take := func(ev map[string]any) {
		tr := cur.tr
		ok, err := f.Match(ev)
		if err != nil {
			w.t.d.event("trigger", "warn", tr.Task, "%s: if: %v", tr, err)
			return
		}
		if !ok {
			return
		}
		gather := tr.Batch + tr.Debounce
		if gather == 0 {
			w.t.fireEvent(cur, ev, 0)
			return
		}
		batch = append(batch, ev)
		if timer == nil || tr.Debounce > 0 {
			timer = time.After(gather)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.events:
			take(ev)
		case <-timer:
			flush()
		case u := <-w.updates:
			if u.changed {
				flush()
			}
			cur, f = u.b, u.f
			close(u.ack)
		case <-w.quit:
			for {
				select {
				case ev := <-w.events:
					take(ev)
				default:
					flush()
					return
				}
			}
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

// source runs the program of the watch w as the daemon runs a service: in
// a session of its own, started again with backoff when it exits, prepared
// again each time from the snapshot that the watch has then, with its
// inputs read again; each line it writes is an event.
func (t *triggers) source(ctx context.Context, w *watcher, spec *watchSpec) error {
	tr := w.s.cur.Load().tr
	svc := &engine.Service{Address: spec.addr, Dir: spec.dir, Program: spec.prog, Restart: "always", Stop: plugin.ServiceStop,
		Rebuild: func() (*plugin.Program, error) {
			b := w.s.cur.Load()
			p, err := engine.WatchSource(b.snap.opts, b.tr, spec.dir)
			if err == nil {
				w.mu.Lock()
				w.prog = p
				w.mu.Unlock()
			}
			return p, err
		},
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
			w.push(ev)
		}}
	w.mu.Lock()
	w.svc = svc
	w.closeFn = func() { t.d.sup.stopSpec(svc) }
	w.mu.Unlock()
	return t.d.sup.Start(ctx, svc)
}

// fireEvent makes a run of the trigger of b for an event with fields, with
// the snapshot of b, unless its task has MaxPending runs unfinished, and
// lets the after triggers know how it ended; depth is how deep in a chain
// of after triggers it is.
func (t *triggers) fireEvent(b *bound, fields map[string]any, depth int) {
	tr := b.tr
	if err := t.begin(tr.Task); err != nil {
		if errors.Is(err, errFull) {
			t.d.event("diagnostic", "warn", tr.Task, "%s: dropped an event, since task %s has %d runs unfinished", tr, tr.Task, MaxPending)
		}
		return
	}
	go func() {
		defer t.end(tr.Task)
		at := time.Now().UTC()
		opts := b.snap.opts
		opts.Event = &engine.TriggerEvent{Trigger: tr.Kind, Time: at, Fields: fields}
		opts.Supervisor = t.d.sup
		res, err := engine.Fire(t.d.runs, opts, tr)
		t.report(tr, at, res, err)
		if res != nil {
			t.ended(res.Record, depth)
		}
	}()
}

// ended makes the runs of the after triggers installed last that the run
// that ended matches, by the record it finished with, which a run that
// only read does not keep on disk; depth is how deep in a chain that run
// is.
func (t *triggers) ended(rec *record.Record, depth int) {
	if rec == nil {
		return
	}
	run := rec.Run
	name := rec.Task
	if name == "" {
		name = rec.Command
	}
	t.mu.Lock()
	snap := t.snap
	t.mu.Unlock()
	if snap == nil {
		return
	}
	for _, tr := range snap.afters {
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
		t.fireEvent(&bound{tr: tr, snap: snap}, map[string]any{"task": name, "unit": tr.Unit, "outcome": rec.Outcome, "outputs": outputs}, depth+1)
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
