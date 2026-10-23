// Package record is the stream of events of a run and what keeps it: the
// envelope of every event, events.jsonl and record.json in the run's
// directory, the limit of stored program output, runs that a crash left
// unfinished, and the retention of old runs.
package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// EventVersion is the version of the shape of events, in v; Version is the
// version of record.json.
const (
	EventVersion = 1
	Version      = 1
)

// MaxOutput is how much program output a run stores; the middle of more is
// dropped, since an error is usually at the end.
const MaxOutput = 10 << 20

// Event is one thing that happened in a run, as every reader gets it.
type Event struct {
	V        int            `json:"v"`
	Seq      int64          `json:"seq"`
	Time     string         `json:"time"`
	Run      string         `json:"run"`
	Type     string         `json:"type"`
	Level    string         `json:"level"`
	Address  string         `json:"address,omitempty"`
	Task     string         `json:"task,omitempty"`
	Step     string         `json:"step,omitempty"`
	Stream   string         `json:"stream,omitempty"`
	Message  string         `json:"message"`
	Outcome  string         `json:"outcome,omitempty"`
	ExitCode *int           `json:"exit_code,omitempty"`
	Outputs  map[string]any `json:"outputs,omitempty"`
}

// Record is record.json: who ran what and when, and how it ended.
type Record struct {
	Version  int              `json:"version"`
	Run      string           `json:"run"`
	Parent   string           `json:"parent,omitempty"`
	Project  string           `json:"project"`
	Stack    string           `json:"stack"`
	Command  string           `json:"command"`
	Task     string           `json:"task,omitempty"`
	Args     []string         `json:"args,omitempty"`
	Trigger  string           `json:"trigger,omitempty"` // what made the run, as event.trigger says
	User     string           `json:"user"`
	Host     string           `json:"host"`
	Hostname string           `json:"hostname"`
	PID      int              `json:"pid"`
	Started  string           `json:"started"`
	Ended    string           `json:"ended,omitempty"`
	Status   string           `json:"status"`
	Outcome  string           `json:"outcome,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	ExitCode int              `json:"exit_code"`
	Units    map[string]*Unit `json:"units,omitempty"`
	Tasks    map[string]*Task `json:"tasks,omitempty"` // the tasks of the project that ran inside the run, by name
	Outputs  map[string]any   `json:"outputs,omitempty"`
}

// Task is how the calls of one task inside a run ended, as one outcome.
type Task struct {
	Outcome string `json:"outcome"`
}

// Unit is what a run did to one unit.
type Unit struct {
	Outcome  string `json:"outcome"`
	Changed  bool   `json:"changed"`
	Plan     string `json:"plan,omitempty"`
	Approval string `json:"approval,omitempty"`
}

// Status of a record.
const (
	Running  = "running"
	Finished = "finished"
)

// Run is the record of one run while it is written. Its directory holds
// .lock, which the run holds while it lives, so a later run knows that a
// record still running was left by a crash; the lock is not inherited by
// programs.
type Run struct {
	Dir string
	rec *Record

	mu     sync.Mutex
	seq    int64
	events *os.File
	out    *bufio.Writer // buffers events.jsonl, flushed by what is not program output
	lock   *os.File
	sink   func(Event)
	stored int64   // program output written
	tail   []Event // events kept in memory once the head of the output is full
	inTail int64   // program output in tail
	gone   int64   // program output dropped from the middle
	flush  *time.Timer
	closed bool
}

// flushEvery is how long buffered events wait before they are written, so
// a reader that follows the run sees them soon.
var flushEvery = 200 * time.Millisecond

// reads are the commands whose runs only read, so their directory goes
// once they finish: the terminal and --json had every event, and up keeps
// its own plan.
var reads = []string{"plan", "status", "logs"}

// Create makes the directory of a new run in runs/<stack>/ under root, with
// an exclusive mkdir so two runs never share a name, and writes rec as
// running. newID names a run; it is asked again when a name is taken.
func Create(root string, newID func() string, rec *Record, sink func(Event)) (*Run, error) {
	base := filepath.Join(root, "runs", rec.Stack)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		id := newID()
		dir := filepath.Join(base, id)
		err := os.Mkdir(dir, 0o700)
		if errors.Is(err, fs.ErrExist) && i < 10 {
			continue
		}
		if err != nil {
			return nil, err
		}
		rec.Run, rec.Status, rec.Version = id, Running, Version
		r := &Run{Dir: dir, rec: rec, sink: sink}
		if r.lock, err = os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600); err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(r.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, err
		}
		if r.events, err = os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
			return nil, err
		}
		r.out = bufio.NewWriterSize(r.events, 64<<10)
		live.Store(rec.Run, &Record{Run: rec.Run, Command: rec.Command, Task: rec.Task, Status: Running})
		return r, write(dir, rec)
	}
}

// live are the runs of this process that have not finished, which are
// alive without a look at their locks.
var live sync.Map // run → what Prune needs of its record

// ID is the name of the run.
func (r *Run) ID() string { return r.rec.Run }

// Task is the task the run was started with.
func (r *Run) Task() string { return r.rec.Task }

// Emit gives an event its envelope, writes it to events.jsonl, and hands
// it to the sink, which gets every event. Past MaxOutput/2 of program
// output, events wait in memory, where the oldest program output beyond
// the other half is dropped; the rest is written when the run finishes.
func (r *Run) Emit(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	ev.V, ev.Seq, ev.Run = EventVersion, r.seq, r.rec.Run
	ev.Time = now()
	if r.sink != nil {
		r.sink(ev)
	}
	size := int64(0)
	if ev.Type == "log" {
		size = int64(len(ev.Message))
	}
	if r.tail == nil && r.stored+size <= MaxOutput/2 {
		r.stored += size
		r.writeEvent(ev)
		return
	}
	r.tail = append(r.tail, ev)
	r.inTail += size
	for i := 0; r.inTail > MaxOutput/2 && i < len(r.tail); {
		if r.tail[i].Type != "log" {
			i++
			continue
		}
		n := int64(len(r.tail[i].Message))
		r.inTail -= n
		r.gone += n
		r.tail = append(r.tail[:i], r.tail[i+1:]...)
	}
}

// writeEvent buffers an event; one that reports a problem or a result is
// written out at once with what came before it, and the others within
// flushEvery, so a crash loses at most the program output and plan lines
// of that time. The caller holds mu.
func (r *Run) writeEvent(ev Event) {
	data, err := json.Marshal(ev)
	if err == nil {
		_, _ = r.out.Write(append(data, '\n'))
	}
	if ev.Level != "info" || (ev.Type != "log" && ev.Type != "plan") {
		_ = r.out.Flush()
		return
	}
	if r.flush == nil {
		r.flush = time.AfterFunc(flushEvery, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.flush = nil
			if !r.closed {
				_ = r.out.Flush()
			}
		})
	}
}

// Finish writes what waited in memory, after one diagnostic event when
// program output was dropped, and the record as finished. What only the
// living run needs goes then: its lock, and artifacts/ when empty; and the
// directory of a run that only read goes whole.
func (r *Run) Finish(update func(*Record)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone > 0 {
		r.seq++
		r.writeEvent(Event{V: EventVersion, Seq: r.seq, Run: r.rec.Run, Time: now(),
			Type: "diagnostic", Level: "warn",
			Message: fmt.Sprintf("%d bytes of program output from the middle of the run were not stored; the run stores %d MiB", r.gone, MaxOutput>>20)})
	}
	for _, ev := range r.tail {
		r.writeEvent(ev)
	}
	r.tail = nil
	_ = r.out.Flush()
	update(r.rec)
	r.rec.Status = Finished
	r.rec.Ended = time.Now().UTC().Format(time.RFC3339)
	err := write(r.Dir, r.rec)
	if r.flush != nil {
		r.flush.Stop()
		r.flush = nil
	}
	r.closed = true
	if cerr := r.events.Close(); err == nil {
		err = cerr
	}
	// The record says finished before the lock goes, so a run that reads
	// it under the lock never takes it for one a crash left.
	_ = r.lock.Close()
	_ = os.Remove(r.lock.Name())
	_ = os.Remove(filepath.Join(r.Dir, "artifacts"))
	live.Delete(r.rec.Run)
	if slices.Contains(reads, r.rec.Command) {
		if rerr := os.RemoveAll(r.Dir); err == nil {
			err = rerr
		}
	}
	return err
}

// Record is the record of the run as it stands, as Finish left it once the
// run finished, also when its directory went.
func (r *Run) Record() *Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := *r.rec
	return &rec
}

// write replaces record.json in dir atomically.
func write(dir string, rec *Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".record.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "record.json"))
}

// ReadRun hands the events of the run in dir to fn, in order. With
// follow, it goes on with what the run writes until the run ended, as
// tail -f --pid does, or until ctx ends.
func ReadRun(ctx context.Context, dir string, follow bool, fn func(Event)) error {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var rest []byte
	for {
		// Whether the run ended is known before its events are read, so
		// all that it wrote before it ended is read below.
		ended := !follow || runEnded(dir)
		if err := readEvents(f, &rest, fn); err != nil {
			return err
		}
		if ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(FollowEvery):
		}
	}
}

// runEnded reports whether the run in dir ended: its record is finished,
// its directory went, or no process holds its lock, which it takes before
// it writes its record.
func runEnded(dir string) bool {
	rec, err := Read(dir)
	switch {
	case err != nil:
		_, serr := os.Stat(dir)
		return serr != nil
	case rec.Status == Finished:
		return true
	}
	if _, ok := live.Load(filepath.Base(dir)); ok {
		return false
	}
	return !held(filepath.Join(dir, ".lock"))
}

// Read reads the record.json of a run.
func Read(dir string) (*Record, error) {
	data, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "record.json"), err)
	}
	if rec.Version > Version {
		return nil, fmt.Errorf("%s is version %d, written by a newer OpsTruss that this one does not read", filepath.Join(dir, "record.json"), rec.Version)
	}
	return &rec, nil
}
