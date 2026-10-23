package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// MaxPending is how many runs of one task the daemon keeps unfinished,
// running or waiting; what comes when a task has them all is dropped.
const MaxPending = 100

// grace is how late a schedule may come and still be on time; one that
// comes later was missed while the machine slept or the clock jumped.
var grace = time.Minute

// Schedule is a schedule or every trigger in <stack>.json: when it last
// made a run, which a persistent schedule catches up from, and when it
// comes next, which list shows.
type Schedule struct {
	Task string `json:"task"`
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Last string `json:"last,omitempty"`
	Next string `json:"next,omitempty"`
}

// triggers are the triggers that the daemon installed. Each install reads
// the project into a snapshot, with the inputs and environment of whoever
// installed it, and reconciles the sources with it, as compose up keeps the
// containers whose config stays: a source whose definition stays lives on,
// with its timer, baseline, or program, and takes the new snapshot for the
// events that come after it.
type triggers struct {
	d     *Daemon
	hooks *webhooks

	// refresh holds one install at a time, from reading the files to the
	// switch, so two never reconcile the same sources; close holds it too.
	refresh sync.Mutex

	mu      sync.Mutex
	closed  bool
	snap    *snapshot          // the one installed last
	sources map[string]*source // by id
	pending map[string]int     // unfinished runs by task
	runs    sync.WaitGroup
}

// snapshot is the project as one install read it, with the inputs and
// environment of that install. An event is handled with one snapshot from
// its if: to its run, and a run keeps the snapshot it began with.
type snapshot struct {
	opts   engine.Options // with Model
	afters []*engine.Trigger
}

// same reports whether events handled with s and o end the same: the same
// project, inputs, and environment.
func (s *snapshot) same(o *snapshot) bool {
	return s != nil && o != nil && reflect.DeepEqual(s.opts.Model, o.opts.Model) && maps.Equal(s.opts.Inputs, o.opts.Inputs) &&
		slices.Equal(slices.Sorted(slices.Values(s.opts.Environ)), slices.Sorted(slices.Values(o.opts.Environ)))
}

// bound is a trigger of a snapshot, whose Index is its place in that
// snapshot's model.
type bound struct {
	tr   *engine.Trigger
	snap *snapshot
}

// source is an installed schedule, every, or watch trigger, which lives
// across installs while its definition stays: its timer, or the watcher,
// program, or lines of its watch.
type source struct {
	id     string
	cur    atomic.Pointer[bound]
	flight bool // a run that it made runs or waits; under triggers.mu
	stop   context.CancelFunc
	done   chan struct{} // closed when it ended
	w      *watcher      // of a watch
}

// sourceIDs names each schedule, every, and watch trigger of list apart
// from its place in the list: by its task, kind, and Key, and by order
// among those that are alike.
func sourceIDs(list []*engine.Trigger) map[*engine.Trigger]string {
	ids, seen := map[*engine.Trigger]string{}, map[string]int{}
	for _, tr := range list {
		if tr.Kind != "schedule" && tr.Kind != "every" && tr.Kind != "watch" {
			continue
		}
		id := tr.Task + "/" + tr.Kind + "/" + tr.Key
		if n := seen[id]; n > 0 {
			ids[tr] = id + "-" + strconv.Itoa(n)
		} else {
			ids[tr] = id
		}
		seen[id]++
	}
	return ids
}

// install reads the project of opts and reconciles the triggers installed
// with it: a source whose definition stays is kept, one that changed is
// stopped and started again, one that is gone stops, and the rest takes
// the new snapshot. What it cannot read or prepare keeps the triggers
// installed before. It returns once each watch that it started is armed,
// and each that it kept took the new snapshot.
func (t *triggers) install(opts engine.Options) {
	t.refresh.Lock()
	defer t.refresh.Unlock()
	t.mu.Lock()
	closed, prev, old := t.closed, t.snap, t.sources
	t.mu.Unlock()
	if closed {
		return
	}
	p, err := model.Load(opts.Dir, opts.Stack)
	var list []*engine.Trigger
	var listen string
	if err == nil {
		opts.Model = p
		list, listen, err = engine.Triggers(opts)
	}
	if err != nil {
		t.d.event("trigger", "warn", "", "the triggers installed before stay: %v", err)
		return
	}
	snap := &snapshot{opts: opts}
	for _, tr := range list {
		if tr.Kind == "after" {
			snap.afters = append(snap.afters, tr)
		}
	}
	changed := !prev.same(snap)
	type planned struct {
		id   string
		s    *source
		b    *bound
		spec *watchSpec
	}
	var keeps, starts []planned
	var stops []*source
	ids := sourceIDs(list)
	for _, tr := range list {
		id, ok := ids[tr]
		if !ok {
			continue
		}
		b := &bound{tr: tr, snap: snap}
		var spec *watchSpec
		if tr.Kind == "watch" {
			if spec, err = t.prepareWatch(id, b); err != nil {
				t.d.event("trigger", "warn", tr.Task, "the triggers installed before stay: %s: %v", tr, err)
				return
			}
		}
		if s := old[id]; s != nil {
			if s.w == nil || s.w.same(spec) {
				keeps = append(keeps, planned{id, s, b, spec})
				continue
			}
			stops = append(stops, s)
		}
		starts = append(starts, planned{id: id, b: b, spec: spec})
	}
	for _, s := range old {
		if !slices.ContainsFunc(keeps, func(k planned) bool { return k.s == s }) && !slices.Contains(stops, s) {
			stops = append(stops, s)
		}
	}
	// What stops goes first, with the events it took, since what takes its
	// place may use its address and directory.
	var wg sync.WaitGroup
	for _, s := range stops {
		wg.Go(func() { t.stopSource(s, true) })
	}
	wg.Wait()
	sources := map[string]*source{}
	for _, k := range keeps {
		sources[k.id] = k.s
	}
	t.mu.Lock()
	t.snap, t.sources = snap, sources
	t.mu.Unlock()
	_ = t.d.reg.update(func(f *File) {
		var keep []*Schedule
		for _, tr := range list {
			if tr.Kind != "schedule" && tr.Kind != "every" {
				continue
			}
			s := &Schedule{Task: tr.Task, Kind: tr.Kind, Key: tr.Key}
			for _, old := range f.Schedules {
				if old.Task == s.Task && old.Kind == s.Kind && old.Key == s.Key {
					s.Last, s.Next = old.Last, old.Next
				}
			}
			keep = append(keep, s)
		}
		f.Schedules = keep
	})
	for _, k := range keeps {
		k.s.cur.Store(k.b)
		if k.s.w != nil {
			k.s.w.update(k.b, k.spec.filter, changed)
		}
	}
	// A change made after install returns, and so after up does, is an
	// event: each watch is armed before it returns.
	var armed sync.WaitGroup
	var failed atomic.Int64
	for _, st := range starts {
		var ready func(error)
		if st.b.tr.Kind == "watch" {
			armed.Add(1)
			var once sync.Once
			ready = func(err error) {
				once.Do(func() {
					if err != nil {
						failed.Add(1)
						t.d.event("trigger", "error", st.b.tr.Task, "%s: %v", st.b.tr, err)
					}
					armed.Done()
				})
			}
		}
		t.start(st.id, st.b, st.spec, ready)
	}
	armed.Wait()
	routes := map[string]*bound{}
	for _, tr := range list {
		if tr.Kind == "webhook" {
			routes[tr.Method+" "+tr.Path] = &bound{tr: tr, snap: snap}
		}
	}
	if err := t.hooks.set(listen, routes); err != nil {
		t.d.event("trigger", "error", "", "%v", err)
	}
	if len(list) > 0 || len(stops) > 0 {
		t.d.event("trigger", "info", "", "installed %d triggers: %d sources started, %d kept, %d stopped, %d failed",
			len(list), len(starts)-int(failed.Load()), len(keeps), len(stops), failed.Load())
	}
}

// start starts the source id of b, with the watch that spec prepared; a
// watch calls ready once it is armed, or with why it is not.
func (t *triggers) start(id string, b *bound, spec *watchSpec, ready func(error)) {
	ctx, stop := context.WithCancel(t.d.runs)
	s := &source{id: id, stop: stop, done: make(chan struct{})}
	s.cur.Store(b)
	if b.tr.Kind == "watch" {
		s.w = newWatcher(t, s, spec)
	}
	t.mu.Lock()
	t.sources[id] = s
	t.mu.Unlock()
	go func() {
		defer close(s.done)
		switch b.tr.Kind {
		case "schedule":
			t.schedule(ctx, s)
		case "every":
			t.every(ctx, s)
		case "watch":
			t.watch(ctx, s, spec, ready)
		}
	}()
}

// stopSource stops s and waits until it ended; with drain, a watch first
// handles the events that it took, so a source that goes takes no event
// after that but loses none it had.
func (t *triggers) stopSource(s *source, drain bool) {
	if s.w != nil {
		s.w.close()
		if drain {
			s.w.drain()
		}
	}
	s.stop()
	<-s.done
}

// forget takes s out of the sources, unless another took its id, so that
// the next install starts it again.
func (t *triggers) forget(s *source) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sources[s.id] == s {
		delete(t.sources, s.id)
	}
}

var (
	errStopping = errors.New("the daemon stops")
	errFull     = errors.New("too many runs unfinished")
)

// begin counts a run of task as unfinished, unless it has MaxPending or
// the daemon stops; end counts it finished.
func (t *triggers) begin(task string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.beginLocked(task)
}

func (t *triggers) beginLocked(task string) error {
	switch {
	case t.closed:
		return errStopping
	case t.pending[task] >= MaxPending:
		return errFull
	}
	t.pending[task]++
	t.runs.Add(1)
	return nil
}

func (t *triggers) end(task string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending[task]--; t.pending[task] == 0 {
		delete(t.pending, task)
	}
	t.runs.Done()
}

// report writes how the run that tr made for the time at ended.
func (t *triggers) report(tr *engine.Trigger, at time.Time, res *engine.Result, err error) {
	switch {
	case err != nil:
		t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
	case res == nil:
		t.d.event("trigger", "info", tr.Task, "%s: if: left the time of %s out", tr, at.Format(time.RFC3339))
	default:
		t.d.event("trigger", "info", tr.Task, "%s: run %s exited with %d", tr, res.Run, res.ExitCode)
	}
}

// close stops the sources, and the webhooks, and waits for the runs that
// they made, which the daemon cancelled; no install comes after it.
func (t *triggers) close() {
	t.refresh.Lock()
	t.mu.Lock()
	t.closed = true
	sources := slices.Collect(maps.Values(t.sources))
	t.sources = nil
	t.mu.Unlock()
	t.refresh.Unlock()
	var wg sync.WaitGroup
	for _, s := range sources {
		wg.Go(func() { t.stopSource(s, false) })
	}
	wg.Wait()
	t.hooks.mu.Lock()
	t.hooks.close()
	t.hooks.mu.Unlock()
	t.runs.Wait()
}

// active reports whether a run that a trigger made runs or waits.
func (t *triggers) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending) > 0
}

// schedule makes a run at each time that the cron of tr names. The daemon
// looks at the clock at least once a minute, since the clock that timers
// count stops while the machine sleeps; a time that comes later than
// grace was missed, and only a persistent schedule runs once for what it
// missed, as Persistent=true of a systemd timer does.
func (t *triggers) schedule(ctx context.Context, s *source) {
	tr := s.cur.Load().tr
	c, err := parseCron(tr.Cron, tr.Timezone)
	if err != nil {
		t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
		return
	}
	now := time.Now()
	if last, ok := t.last(tr); ok && tr.Persistent {
		if missed := lastBefore(c, last, now); !missed.IsZero() {
			t.d.event("trigger", "info", tr.Task, "%s: catching up the run of %s, missed while the daemon did not run", tr, missed.Format(time.RFC3339))
			t.fire(s, missed)
		}
	}
	for next := c.next(now); !next.IsZero(); next = c.next(now) {
		t.setNext(tr, next)
		for now = time.Now(); now.Before(next); now = time.Now() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(next.Sub(now), time.Minute)):
			}
		}
		// persistent is the rule of the snapshot installed last.
		switch missed := lastBefore(c, next.Add(-time.Nanosecond), now); {
		case now.Sub(next) <= grace:
			t.fire(s, next)
		case s.cur.Load().tr.Persistent:
			t.d.event("trigger", "info", tr.Task, "%s: catching up the run of %s, missed while the machine slept", tr, missed.Format(time.RFC3339))
			t.fire(s, missed)
		default:
			t.d.event("trigger", "warn", tr.Task, "%s: missed the run of %s while the machine slept", tr, missed.Format(time.RFC3339))
		}
	}
	t.d.event("trigger", "warn", tr.Task, "%s comes no more", tr)
}

// lastBefore is the last time that c names after from and up to now, or
// the zero time.
func lastBefore(c *cronSpec, from, now time.Time) time.Time {
	var last time.Time
	for at := c.next(from); !at.IsZero() && !at.After(now); at = c.next(at) {
		last = at
	}
	return last
}

// every makes a run one interval after it is installed, and each interval
// after that; an install that keeps it keeps its ticks.
func (t *triggers) every(ctx context.Context, s *source) {
	tr := s.cur.Load().tr
	tick := time.NewTicker(tr.Every)
	defer tick.Stop()
	t.setNext(tr, time.Now().Add(tr.Every))
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-tick.C:
			t.setNext(tr, at.Add(tr.Every))
			t.fire(s, at)
		}
	}
}

// fire makes a run of the trigger of s for the time at, with the snapshot
// that s has, unless the run it made last still runs or waits, which takes
// this time too, as a systemd timer does not start a service that is
// active, or unless its task has MaxPending runs unfinished.
func (t *triggers) fire(s *source, at time.Time) {
	b := s.cur.Load()
	tr := b.tr
	t.mu.Lock()
	inFlight := s.flight
	var err error
	if !inFlight {
		if err = t.beginLocked(tr.Task); err == nil {
			s.flight = true
		}
	}
	t.mu.Unlock()
	switch {
	case inFlight:
		t.d.event("trigger", "info", tr.Task, "%s: the run it made last runs or waits still, and takes the time of %s too", tr, at.Format(time.RFC3339))
		return
	case errors.Is(err, errFull):
		t.d.event("diagnostic", "warn", tr.Task, "%s: dropped the time of %s, since task %s has %d runs unfinished", tr, at.Format(time.RFC3339), tr.Task, MaxPending)
		return
	case err != nil:
		return
	}
	if tr.Kind == "schedule" {
		t.setLast(tr, at)
	}
	go func() {
		defer func() {
			t.mu.Lock()
			s.flight = false
			t.mu.Unlock()
			t.end(tr.Task)
		}()
		opts := b.snap.opts
		opts.Event = &engine.TriggerEvent{Trigger: tr.Kind, Time: at.UTC()}
		opts.Supervisor = t.d.sup
		res, err := engine.Fire(t.d.runs, opts, tr)
		t.report(tr, at, res, err)
		if res != nil {
			t.ended(res.Record, 0)
		}
	}()
}

func (t *triggers) last(tr *engine.Trigger) (time.Time, bool) {
	for _, s := range t.d.reg.files().Schedules {
		if s.Task == tr.Task && s.Kind == tr.Kind && s.Key == tr.Key && s.Last != "" {
			at, err := time.Parse(time.RFC3339, s.Last)
			return at, err == nil
		}
	}
	return time.Time{}, false
}

func (t *triggers) setLast(tr *engine.Trigger, at time.Time) {
	t.setSchedule(tr, func(s *Schedule) { s.Last = at.UTC().Format(time.RFC3339) })
}

func (t *triggers) setNext(tr *engine.Trigger, at time.Time) {
	t.setSchedule(tr, func(s *Schedule) { s.Next = at.UTC().Format(time.RFC3339) })
}

func (t *triggers) setSchedule(tr *engine.Trigger, fn func(*Schedule)) {
	_ = t.d.reg.update(func(f *File) {
		for i, s := range f.Schedules {
			if s.Task == tr.Task && s.Kind == tr.Kind && s.Key == tr.Key {
				next := *s
				fn(&next)
				f.Schedules[i] = &next
			}
		}
	})
}

// Next returns when each task of stack comes next by its schedule and
// every triggers, while its daemon runs, as list shows it.
func Next(root, stack string) map[string]time.Time {
	if !Alive(root, stack) {
		return nil
	}
	f, err := Read(root, stack)
	if err != nil {
		return nil
	}
	out := map[string]time.Time{}
	for _, s := range f.Schedules {
		at, err := time.Parse(time.RFC3339, s.Next)
		if err != nil {
			continue
		}
		if old, ok := out[s.Task]; !ok || at.Before(old) {
			out[s.Task] = at
		}
	}
	return out
}

// event writes an event of the daemon to its log, and to the terminal of
// a daemon in the foreground.
func (d *Daemon) event(typ, level, task, format string, args ...any) {
	ev := record.Event{Type: typ, Level: level, Task: task, Message: fmt.Sprintf(format, args...)}
	d.log.Emit(ev)
	if d.opts.Watch != nil {
		d.opts.Watch(ev)
	}
}
