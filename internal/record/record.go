// Package record is the stream of events of a run and what keeps it: the
// envelope of every event, events.jsonl and record.json in the run's
// directory, the limit of stored program output, runs that a crash left
// unfinished, and the retention of old runs.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	Outputs  map[string]any   `json:"outputs,omitempty"`
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
	lock   *os.File
	sink   func(Event)
	stored int64   // program output written
	tail   []Event // events kept in memory once the head of the output is full
	inTail int64   // program output in tail
	gone   int64   // program output dropped from the middle
}

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
		return r, write(dir, rec)
	}
}

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
	ev.Time = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
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

func (r *Run) writeEvent(ev Event) {
	data, err := json.Marshal(ev)
	if err == nil {
		_, _ = r.events.Write(append(data, '\n'))
	}
}

// Finish writes what waited in memory, after one diagnostic event when
// program output was dropped, and the record as finished.
func (r *Run) Finish(update func(*Record)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone > 0 {
		r.seq++
		r.writeEvent(Event{V: EventVersion, Seq: r.seq, Run: r.rec.Run, Time: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			Type: "diagnostic", Level: "warn",
			Message: fmt.Sprintf("%d bytes of program output from the middle of the run were not stored; the run stores %d MiB", r.gone, MaxOutput>>20)})
	}
	for _, ev := range r.tail {
		r.writeEvent(ev)
	}
	r.tail = nil
	update(r.rec)
	r.rec.Status = Finished
	r.rec.Ended = time.Now().UTC().Format(time.RFC3339)
	err := write(r.Dir, r.rec)
	if cerr := r.events.Close(); err == nil {
		err = cerr
	}
	_ = r.lock.Close()
	return err
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
