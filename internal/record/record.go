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

	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/state"
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
	Plan     map[string]any `json:"plan,omitempty"` // of a plan event: the plan of its unit, as units.<n>.plan has it
}

// Record is record.json: who ran what and when, and how it ended.
type Record struct {
	Version   int              `json:"version"`
	Run       string           `json:"run"`
	Parent    string           `json:"parent,omitempty"`
	Project   string           `json:"project"`
	Stack     string           `json:"stack"`
	Command   string           `json:"command"`
	Task      string           `json:"task,omitempty"`
	Args      []string         `json:"args,omitempty"`
	Trigger   string           `json:"trigger,omitempty"`   // what made the run, as event.trigger says
	Simulated string           `json:"simulated,omitempty"` // with --event: the trigger whose event the caller wrote
	User      string           `json:"user"`
	Host      string           `json:"host"`
	Hostname  string           `json:"hostname"`
	PID       int              `json:"pid"`
	Started   string           `json:"started"`
	Ended     string           `json:"ended,omitempty"`
	Status    string           `json:"status"`
	Outcome   string           `json:"outcome,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	ExitCode  int              `json:"exit_code"`
	Units     map[string]*Unit `json:"units,omitempty"`
	Tasks     map[string]*Task `json:"tasks,omitempty"` // the tasks of the project that ran inside the run, by name
	Outputs   map[string]any   `json:"outputs,omitempty"`
	// EventsIncomplete says that events.jsonl lacks events that could not
	// be written, as on a full disk.
	EventsIncomplete bool `json:"events_incomplete,omitempty"`
	NoDeps           bool `json:"no_deps,omitempty"`
	// RerunOf is the run that opstruss runs rerun ran again.
	RerunOf string `json:"rerun_of,omitempty"`
	// Replay is what runs rerun needs beside the record, of a run of a
	// task, up, or down; a record without it cannot run again.
	Replay *Replay `json:"replay,omitempty"`
}

// Replay is how a run can run again: the trigger whose event made it, and
// whether what came from outside the project, which may hold secrets, is
// sealed in the run's replay file.
type Replay struct {
	Trigger string `json:"trigger,omitempty"` // the key of that trigger in the project file
	Time    string `json:"time,omitempty"`    // event.time
	Payload string `json:"payload,omitempty"` // sealed, or unavailable as Error says; empty when nothing is needed
	Error   string `json:"error,omitempty"`
}

// Replay payloads.
const (
	Sealed      = "sealed"
	Unavailable = "unavailable"
)

// replayFile holds the sealed payload of a run beside its record.
const replayFile = "replay"

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
	done   chan struct{} // closed with the run, which ends WatchCancel
	err    error         // the first event that could not be written
	cut    bool          // a write failed, maybe in the middle of a line
	// committed is set once record.json says finished; until then the run
	// keeps its .lock file, so the next run marks it.
	committed bool
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
		r := &Run{Dir: dir, rec: rec, sink: sink, done: make(chan struct{})}
		live.Store(rec.Run, &Record{Run: rec.Run, Command: rec.Command, Task: rec.Task, Status: Running})
		if err := r.open(); err != nil {
			// A run that could not start leaves nothing behind.
			if r.events != nil {
				_ = r.events.Close()
			}
			if r.lock != nil {
				_ = r.lock.Close()
			}
			live.Delete(rec.Run)
			_ = os.RemoveAll(dir)
			return nil, err
		}
		return r, nil
	}
}

// open takes the lock of a new run, opens its events, and writes its
// record as running.
func (r *Run) open() (err error) {
	if r.lock, err = os.OpenFile(filepath.Join(r.Dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return err
	}
	if err := syscall.Flock(int(r.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	if r.events, err = os.OpenFile(filepath.Join(r.Dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
		return err
	}
	r.out = bufio.NewWriterSize(r.events, 64<<10)
	if err := failpoint.Fail("record-events-write"); err != nil {
		// The first events are not written, as on a full disk.
		r.out = bufio.NewWriterSize(refused{err}, 64<<10)
	}
	return write(r.Dir, r.rec)
}

// refused is a file that refuses what is written to it.
type refused struct{ err error }

func (f refused) Write([]byte) (int, error) { return 0, f.err }

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
	r.emit(ev)
}

// emit is Emit for a caller that holds mu.
func (r *Run) emit(ev Event) {
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
	if r.closed {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		r.lose(err)
		return
	}
	if r.cut {
		// The line before may have been cut where the write failed.
		data = append([]byte{'\n'}, data...)
		r.cut = false
	}
	if _, err := r.out.Write(append(data, '\n')); err != nil {
		r.fail(err)
		return
	}
	if ev.Level != "info" || (ev.Type != "log" && ev.Type != "plan") {
		r.fail(r.out.Flush())
		return
	}
	if r.flush == nil {
		r.flush = time.AfterFunc(flushEvery, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.flush = nil
			if !r.closed {
				r.fail(r.out.Flush())
			}
		})
	}
}

// lose keeps the first event that could not be written, which marks the
// record.
func (r *Run) lose(err error) {
	if r.err == nil {
		r.err = err
	}
}

// fail is lose for a write of events.jsonl that failed: the writer starts
// over without what it held, so events are stored again once the disk
// takes them.
func (r *Run) fail(err error) {
	if err == nil {
		return
	}
	r.lose(err)
	r.out.Reset(r.events)
	r.cut = true
}

// Finish writes what waited in memory, after one diagnostic event when
// program output was dropped, then the record as finished, while the run
// still holds its lock: the last event of the run comes next, then Close.
// Events that could not be written mark the record, after one diagnostic
// event that says so; the error is that of the record alone.
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
	r.fail(r.out.Flush())
	if r.err != nil {
		r.emit(Event{Type: "diagnostic", Level: "warn", Message: fmt.Sprintf("not every event of the run was stored: %v", r.err)})
	}
	update(r.rec)
	r.rec.Status, r.rec.EventsIncomplete = Finished, r.err != nil
	r.rec.Ended = time.Now().UTC().Format(time.RFC3339)
	if err := write(r.Dir, r.rec); err != nil {
		return err
	}
	r.committed = true
	return nil
}

// Close ends the run after its last event: what waits is written, and the
// lock goes, and with it what only the living run needed: artifacts/ when
// empty, and the whole directory of a run that only read. A run whose
// record Finish could not write keeps its .lock file, so the next run
// marks it.
func (r *Run) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flush != nil {
		r.flush.Stop()
		r.flush = nil
	}
	r.fail(r.out.Flush())
	if err := r.events.Close(); err != nil {
		r.lose(err)
	}
	if !r.closed {
		close(r.done)
	}
	r.closed = true
	if r.committed && r.err != nil && !r.rec.EventsIncomplete {
		// The last event was lost; the record says so if it can.
		r.rec.EventsIncomplete = true
		_ = write(r.Dir, r.rec)
	}
	_ = r.lock.Close()
	live.Delete(r.rec.Run)
	if !r.committed {
		return
	}
	// The record says finished before the lock goes, so a run that reads
	// it under the lock never takes it for one a crash left.
	_ = os.Remove(r.lock.Name())
	_ = os.Remove(filepath.Join(r.Dir, "artifacts"))
	if slices.Contains(reads, r.rec.Command) {
		// What a crash or a failure leaves, retention drops later.
		_ = os.RemoveAll(r.Dir)
	}
}

// WriteReplay writes the sealed payload of the run.
func (r *Run) WriteReplay(sealed []byte) error {
	return os.WriteFile(filepath.Join(r.Dir, replayFile), sealed, 0o600)
}

// Update changes the record of a run that goes on running.
func (r *Run) Update(update func(*Record)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	update(r.rec)
	return write(r.Dir, r.rec)
}

// ReadReplay reads the sealed payload of the run in dir.
func ReadReplay(dir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, replayFile))
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

// runEnded reports whether the run in dir ended: its directory went, or no
// process holds its lock, which it takes before it writes its record and
// lets go only after its last event. Its record says finished before that
// event, so it does not tell.
func runEnded(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
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
		return nil, &state.NewerError{What: filepath.Join(dir, "record.json"), Version: rec.Version, Known: Version}
	}
	return &rec, nil
}
