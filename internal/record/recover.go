package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// Keep is how many runs of each task a stack keeps, and MaxRuns how much
// runs/<stack>/ takes, oldest dropped first; a task that runs every 30
// seconds would make 2,880 of them a day.
const (
	Keep    = 100
	MaxRuns = 1 << 30
)

// Prune keeps the runs of stack under root in order, at the end of a run.
// First it marks the runs that ended with their record still running: a
// crash, or a record that could not be written. Their .lock is free, or
// gone, as an older OpsTruss removed it then. A run whose events end with
// its run event takes how that says it ended; the others end cancelled,
// as interrupted. Then it drops the oldest runs beyond Keep per task, then
// beyond MaxRuns in all, but never a run still running, nor the last run
// that touched each unit, which logs reads. It returns the records it
// marked, so the run can say so; runs it could not mark or drop are in the
// error, which does not stop it.
func Prune(root, stack string) ([]*Record, error) {
	base := filepath.Join(root, "runs", stack)
	names, ix, err := scan(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer ix.mu.Unlock()
	var marked []*Record
	var add []*entry
	var errs []error
	runs := make([]run, 0, len(names))
	for _, name := range names {
		// A run of this process that runs is kept, and counts among the
		// newest of its task; its record is not read.
		if v, ok := live.Load(name); ok {
			rec := v.(*Record)
			runs = append(runs, run{name: name, command: rec.Command, task: rec.Task, running: true})
			continue
		}
		if e := ix.runs[name]; e != nil {
			runs = append(runs, run{name: name, command: e.Command, task: e.Task, units: e.Units, size: e.Size})
			continue
		}
		// A record that cannot be read is left as it is: runs and list
		// say why.
		dir := filepath.Join(base, name)
		rec, err := Read(dir)
		if err != nil {
			continue
		}
		if rec.Status == Running {
			m, err := mark(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("marking run %s, which ended without its record: %w", name, err))
			}
			if m == nil {
				runs = append(runs, run{name: name, command: rec.Command, task: rec.Task, size: size(dir), running: true})
				continue
			}
			marked, rec = append(marked, m), m
		}
		e := entryOf(dir, rec)
		add = append(add, e)
		runs = append(runs, run{name: name, command: e.Command, task: e.Task, units: e.Units, size: e.Size})
	}
	ix.add(add)
	if err := drop(base, ix, runs); err != nil {
		errs = append(errs, err)
	}
	ix.compact()
	return marked, errors.Join(errs...)
}

// run is what Prune knows of a run.
type run struct {
	name, command, task string
	units               []string
	size                int64
	running             bool
}

// drop drops the oldest of runs, which come oldest first, beyond Keep per
// task, then beyond MaxRuns in all, with a run that only read and a crash
// left first.
func drop(base string, ix *index, runs []run) error {
	keep := map[string]bool{}
	last := map[string]string{}
	for _, r := range runs {
		for _, u := range r.units {
			last[u] = r.name
		}
	}
	for _, name := range last {
		keep[name] = true
	}
	gone := map[string]bool{}
	var total int64
	for _, r := range runs {
		total += r.size
	}
	one := func(r run) error {
		if keep[r.name] || gone[r.name] || r.running {
			return nil
		}
		gone[r.name] = true
		total -= r.size
		delete(ix.runs, r.name)
		if err := os.RemoveAll(filepath.Join(base, r.name)); err != nil {
			return fmt.Errorf("dropping run %s: %w", r.name, err)
		}
		return nil
	}
	// A run that only read is kept while it runs; one that a crash left
	// finished is dropped, as it would have dropped itself.
	for _, r := range runs {
		if slices.Contains(reads, r.command) {
			if err := one(r); err != nil {
				return err
			}
		}
	}
	// The oldest runs of each task beyond Keep go.
	type task struct{ command, task string }
	count, seen := map[task]int{}, map[task]int{}
	for _, r := range runs {
		count[task{r.command, r.task}]++
	}
	for _, r := range runs {
		k := task{r.command, r.task}
		if seen[k]++; seen[k] <= count[k]-Keep {
			if err := one(r); err != nil {
				return err
			}
		}
	}
	for _, r := range runs {
		if total <= MaxRuns {
			break
		}
		if err := one(r); err != nil {
			return err
		}
	}
	return nil
}

// mark writes the record of the run in dir as finished, when no process
// holds its lock: the owner lets go of it only after it wrote its record
// as finished, so the record is read again under the lock. It returns nil
// for a run that still lives.
func mark(dir string) (*Record, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	rec, err := Read(dir)
	if err != nil || rec.Status != Running {
		return nil, err
	}
	rec.Status, rec.Outcome, rec.Reason = Finished, "cancelled", "interrupted"
	rec.Ended = Stamp(time.Now())
	if ev := lastEvent(filepath.Join(dir, "events.jsonl")); ev != nil && ev.Type == "run" {
		rec.Outcome, rec.Reason, rec.Outputs = ev.Outcome, "", ev.Outputs
		if ev.ExitCode != nil {
			rec.ExitCode = *ev.ExitCode
		}
		if t, err := time.Parse(time.RFC3339Nano, ev.Time); err == nil {
			rec.Ended = Stamp(t)
		}
	}
	if err := write(dir, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// lastEvent is the last whole event of the events.jsonl at path, or nil.
func lastEvent(path string) *Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	end := info.Size()
	// The last line starts after the newline before it; one that a crash
	// cut short is not an event.
	for n := int64(64 << 10); ; n *= 4 {
		from := max(end-n, 0)
		buf := make([]byte, end-from)
		if _, err := f.ReadAt(buf, from); err != nil && !errors.Is(err, io.EOF) {
			return nil
		}
		if !bytes.HasSuffix(buf, []byte{'\n'}) {
			return nil
		}
		buf = buf[:len(buf)-1]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 || from == 0 {
			var ev Event
			if json.Unmarshal(buf[i+1:], &ev) != nil || ev.V > EventVersion {
				return nil
			}
			return &ev
		}
	}
}

// Alive reports whether the run named run, of any stack under root, still
// holds the lock of its record.
func Alive(root, run string) bool {
	if _, ok := live.Load(filepath.Base(run)); ok {
		return true
	}
	locks, _ := filepath.Glob(filepath.Join(root, "runs", "*", filepath.Base(run), ".lock"))
	for _, p := range locks {
		if held(p) {
			return true
		}
	}
	return false
}

// held reports whether a process holds the lock at p.
func held(p string) bool {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

func size(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
