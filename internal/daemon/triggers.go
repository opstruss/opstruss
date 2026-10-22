package daemon

import (
	"context"
	"fmt"
	"sync"
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

// triggers are the triggers that the daemon installed: from the project
// as it was then, with the inputs and environment of that time.
type triggers struct {
	d    *Daemon
	opts engine.Options

	mu      sync.Mutex
	stop    context.CancelFunc
	flight  map[string]bool // triggers with a run that runs or waits
	pending map[string]int  // unfinished runs by task
	runs    sync.WaitGroup
}

func keyOf(t *engine.Trigger) string { return t.Task + "/" + t.Kind + "/" + t.Key }

// install reads the project of opts and installs its triggers in place of
// those installed before; runs that those made go on with the project as
// they found it. A project that does not load keeps the triggers there
// are.
func (t *triggers) install(opts engine.Options) {
	p, err := model.Load(opts.Dir, opts.Stack)
	var list []*engine.Trigger
	if err == nil {
		opts.Model = p
		list, err = engine.Triggers(opts)
	}
	if err != nil {
		t.d.event("trigger", "warn", "", "the triggers installed before stay: %v", err)
		return
	}
	ctx, stop := context.WithCancel(t.d.runs)
	t.mu.Lock()
	if t.stop != nil {
		t.stop()
	}
	t.stop, t.opts = stop, opts
	t.mu.Unlock()
	_ = t.d.reg.update(func(f *File) {
		var keep []*Schedule
		for _, tr := range list {
			s := &Schedule{Task: tr.Task, Kind: tr.Kind, Key: tr.Key}
			for _, old := range f.Schedules {
				if old.Task == s.Task && old.Kind == s.Kind && old.Key == s.Key {
					s.Last = old.Last
				}
			}
			keep = append(keep, s)
		}
		f.Schedules = keep
	})
	for _, tr := range list {
		switch tr.Kind {
		case "schedule":
			go t.schedule(ctx, tr)
		case "every":
			go t.every(ctx, tr)
		}
	}
	if len(list) > 0 {
		t.d.event("trigger", "info", "", "installed %d triggers", len(list))
	}
}

// close stops the triggers and waits for the runs they made, which the
// daemon cancelled.
func (t *triggers) close() {
	t.mu.Lock()
	if t.stop != nil {
		t.stop()
	}
	t.mu.Unlock()
	t.runs.Wait()
}

// active reports whether a run that a trigger made runs or waits.
func (t *triggers) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.flight) > 0
}

// schedule makes a run at each time that the cron of tr names. The daemon
// looks at the clock at least once a minute, since the clock that timers
// count stops while the machine sleeps; a time that comes later than
// grace was missed, and only a persistent schedule runs once for what it
// missed, as Persistent=true of a systemd timer does.
func (t *triggers) schedule(ctx context.Context, tr *engine.Trigger) {
	c, err := parseCron(tr.Cron, tr.Timezone)
	if err != nil {
		t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
		return
	}
	now := time.Now()
	if last, ok := t.last(tr); ok && tr.Persistent {
		if missed := lastBefore(c, last, now); !missed.IsZero() {
			t.d.event("trigger", "info", tr.Task, "%s: catching up the run of %s, missed while the daemon did not run", tr, missed.Format(time.RFC3339))
			t.fire(tr, missed)
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
		switch missed := lastBefore(c, next.Add(-time.Nanosecond), now); {
		case now.Sub(next) <= grace:
			t.fire(tr, next)
		case tr.Persistent:
			t.d.event("trigger", "info", tr.Task, "%s: catching up the run of %s, missed while the machine slept", tr, missed.Format(time.RFC3339))
			t.fire(tr, missed)
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
// after that.
func (t *triggers) every(ctx context.Context, tr *engine.Trigger) {
	tick := time.NewTicker(tr.Every)
	defer tick.Stop()
	t.setNext(tr, time.Now().Add(tr.Every))
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-tick.C:
			t.setNext(tr, at.Add(tr.Every))
			t.fire(tr, at)
		}
	}
}

// fire makes a run of tr for the time at, unless the run it made last
// still runs or waits, which takes this time too, as a systemd timer does
// not start a service that is active, or unless its task has MaxPending
// runs unfinished.
func (t *triggers) fire(tr *engine.Trigger, at time.Time) {
	t.mu.Lock()
	key := keyOf(tr)
	switch {
	case t.flight[key]:
		t.mu.Unlock()
		t.d.event("trigger", "info", tr.Task, "%s: the run it made last runs or waits still, and takes the time of %s too", tr, at.Format(time.RFC3339))
		return
	case t.pending[tr.Task] >= MaxPending:
		t.mu.Unlock()
		t.d.event("diagnostic", "warn", tr.Task, "%s: dropped the time of %s, since task %s has %d runs unfinished", tr, at.Format(time.RFC3339), tr.Task, MaxPending)
		return
	}
	t.flight[key] = true
	t.pending[tr.Task]++
	opts := t.opts
	t.mu.Unlock()
	if tr.Kind == "schedule" {
		t.setLast(tr, at)
	}
	t.runs.Add(1)
	go func() {
		defer t.runs.Done()
		defer func() {
			t.mu.Lock()
			delete(t.flight, key)
			t.pending[tr.Task]--
			t.mu.Unlock()
		}()
		opts.Event = &engine.TriggerEvent{Trigger: tr.Kind, Time: at.UTC()}
		opts.Supervisor = t.d.sup
		res, err := engine.Fire(t.d.runs, opts, tr)
		switch {
		case err != nil:
			t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
		case res == nil:
			t.d.event("trigger", "info", tr.Task, "%s: if: left the time of %s out", tr, at.Format(time.RFC3339))
		default:
			t.d.event("trigger", "info", tr.Task, "%s: run %s exited with %d", tr, res.Run, res.ExitCode)
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
