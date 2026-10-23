package record

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/schema"
	"github.com/opstruss/opstruss/internal/state"
)

func ids() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("20261020T%06dZ-%08x", n, n)
	}
}

func valid(t *testing.T, k schema.Kind, data []byte) {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	probs, err := schema.Validate(k, doc)
	if err != nil || len(probs) > 0 {
		t.Fatalf("%s: %v %v\n%s", k, probs, err, data)
	}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	var got []Event
	r, err := Create(root, ids(), &Record{Project: "shop", Stack: "dev", Command: "up", Host: "h", PID: 1, Started: "now"},
		func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatal(err)
	}
	r.Emit(Event{Type: "unit", Level: "info", Address: "dev/web", Message: "up: done"})
	r.Emit(Event{Type: "log", Level: "info", Address: "dev/web", Stream: "stdout", Message: "hello"})
	if err := r.Finish(func(rec *Record) {
		rec.Outcome, rec.Units = "success", map[string]*Unit{"web": {Outcome: "success", Changed: true}}
		rec.Tasks = map[string]*Task{"build": {Outcome: "success"}}
	}); err != nil {
		t.Fatal(err)
	}
	code := 0
	r.Emit(Event{Type: "run", Level: "info", Message: "success", Outcome: "success", ExitCode: &code})
	r.Close()
	if len(got) != 3 || got[2].Seq != 3 || got[0].Run != r.ID() || got[0].V != EventVersion {
		t.Fatalf("the sink got %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(r.Dir, "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	valid(t, schema.Run, data)
	f, err := os.Open(filepath.Join(r.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		valid(t, schema.Event, sc.Bytes())
		n++
	}
	if n != 3 {
		t.Fatalf("events.jsonl has %d events", n)
	}
	for _, p := range []string{r.Dir, filepath.Join(r.Dir, "record.json")} {
		if info, _ := os.Stat(p); info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is %v", p, info.Mode())
		}
	}
}

func TestOutputLimit(t *testing.T) {
	r, err := Create(t.TempDir(), ids(), &Record{Project: "shop", Stack: "dev", Command: "up", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1023)
	total := 0
	for i := 0; total <= MaxOutput+MaxOutput/4; i++ {
		msg := fmt.Sprintf("%06d %s", i, line)
		total += len(msg)
		r.Emit(Event{Type: "log", Level: "info", Stream: "stdout", Message: msg})
		if i == 100 {
			r.Emit(Event{Type: "unit", Level: "info", Message: "kept"})
		}
	}
	r.Emit(Event{Type: "log", Level: "info", Stream: "stdout", Message: "the last line"})
	if err := r.Finish(func(*Record) {}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	data, err := os.ReadFile(filepath.Join(r.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`"000000 x`, `"kept"`, "were not stored", "the last line"} {
		if !strings.Contains(s, want) {
			t.Errorf("events.jsonl has no %q", want)
		}
	}
	if len(data) > MaxOutput+MaxOutput/2 {
		t.Fatalf("events.jsonl takes %d bytes", len(data))
	}
}

func TestInterruptedAndPrune(t *testing.T) {
	root := t.TempDir()
	newID := ids()
	r, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "up", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Prune(root, "dev"); len(got) != 0 {
		t.Fatalf("a live run is interrupted: %v", got)
	}
	// A crash lets go of the lock without finishing the record, and the
	// process that knew the run is gone.
	_ = r.lock.Close()
	live.Delete(r.ID())
	got, err := Prune(root, "dev")
	if err != nil || len(got) != 1 || got[0].Run != r.ID() {
		t.Fatalf("interrupted: %v %v", got, err)
	}
	rec, _ := Read(r.Dir)
	if rec.Status != Finished || rec.Outcome != "cancelled" || rec.Reason != "interrupted" {
		t.Fatalf("record %+v", rec)
	}

	// Keep runs of each task; the last run that touched a unit stays too.
	for i := range Keep + 5 {
		units := map[string]*Unit{}
		if i == 0 {
			units["db"] = &Unit{Outcome: "success"}
		}
		r, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "task", Task: "backup", Host: "h", PID: 1, Started: "now"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Finish(func(rec *Record) { rec.Units = units }); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	running, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "task", Task: "backup", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "runs", "dev", "2*"))
	// The newest 100 of backup, the running one among them, the first
	// backup run, the last to touch db, and the run of up.
	if len(dirs) != Keep+2 {
		t.Fatalf("%d runs are left", len(dirs))
	}
	if _, err := os.Stat(running.Dir); err != nil {
		t.Fatalf("the running run was dropped: %v", err)
	}
}

// A finished run keeps record.json and events.jsonl only, and artifacts/
// when it holds something; a run that only read goes whole, and one that a
// crash left goes with the next Prune.
func TestFinishLeaves(t *testing.T) {
	root := t.TempDir()
	start := func(command string, artifact bool) *Run {
		t.Helper()
		r, err := Create(root, ids(), &Record{Project: "shop", Stack: "dev", Command: command, Host: "h", PID: 1, Started: "now"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(r.Dir, "artifacts"), 0o700); err != nil {
			t.Fatal(err)
		}
		if artifact {
			if err := os.WriteFile(filepath.Join(r.Dir, "artifacts", "report.txt"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	finish := func(r *Run) {
		t.Helper()
		if err := r.Finish(func(rec *Record) { rec.Outcome = "success" }); err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	names := func(dir string) string {
		t.Helper()
		ents, _ := os.ReadDir(dir)
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}

	up := start("up", false)
	finish(up)
	if got := names(up.Dir); got != "events.jsonl,record.json" {
		t.Fatalf("a finished up left %s", got)
	}
	if Alive(root, up.ID()) {
		t.Fatal("a finished run is alive")
	}
	kept := start("task", true)
	finish(kept)
	if got := names(kept.Dir); got != "artifacts,events.jsonl,record.json" {
		t.Fatalf("a run with an artifact left %s", got)
	}
	plan := start("plan", true)
	if rec := plan.Record(); rec.Status != Running {
		t.Fatalf("a plan that runs is %s", rec.Status)
	}
	finish(plan)
	if _, err := os.Stat(plan.Dir); !os.IsNotExist(err) {
		t.Fatalf("a finished plan is still there: %v", err)
	}
	if rec := plan.Record(); rec.Status != Finished || rec.Outcome != "success" {
		t.Fatalf("the record of a plan that went is %+v", rec)
	}

	// What a crash left of a run that read: its record says finished once
	// the next run marked it interrupted, and Prune drops it.
	left := filepath.Join(root, "runs", "dev", "20261020T999999Z-0000ffff")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := write(left, &Record{Version: Version, Run: filepath.Base(left), Project: "shop", Stack: "dev", Command: "status",
		Host: "h", PID: 1, Started: "now", Status: Running}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, ".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Prune(root, "dev"); err != nil || len(got) != 1 {
		t.Fatalf("interrupted %v %v", got, err)
	}
	if _, err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("Prune kept the status a crash left: %v", err)
	}
	if _, err := os.Stat(up.Dir); err != nil {
		t.Fatalf("Prune dropped a kept run: %v", err)
	}
}

// Events that wait in the buffer are written within FlushEvery, so a
// reader that follows the run sees them while it runs.
func TestRunFlushes(t *testing.T) {
	root := t.TempDir()
	r, err := Create(root, ids(), &Record{Project: "shop", Stack: "dev", Command: "task", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Emit(Event{Type: "log", Level: "info", Task: "build", Message: "first"})
	p := filepath.Join(r.Dir, "events.jsonl")
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if data, _ := os.ReadFile(p); strings.Contains(string(data), "first") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a log line waited more than 2s to be written")
		}
	}

	// ReadRun with follow goes on until the run finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan []string, 1)
	go func() {
		var lines []string
		_ = ReadRun(ctx, r.Dir, true, func(ev Event) { lines = append(lines, ev.Message) })
		got <- lines
	}()
	r.Emit(Event{Type: "log", Level: "info", Task: "build", Message: "second"})
	time.Sleep(3 * FollowEvery)
	if err := r.Finish(func(*Record) {}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	select {
	case lines := <-got:
		if strings.Join(lines, ",") != "first,second" {
			t.Fatalf("followed %v", lines)
		}
	case <-ctx.Done():
		t.Fatal("ReadRun did not end with the run")
	}
}

// Events that cannot be written mark the record, which is written, and the
// run says so once; events after the disk takes them again are stored.
func TestEventsLost(t *testing.T) {
	var got []Event
	r, err := Create(t.TempDir(), ids(), &Record{Project: "shop", Stack: "dev", Command: "task", Host: "h", PID: 1, Started: "now"},
		func(ev Event) { got = append(got, ev) })
	if err != nil {
		t.Fatal(err)
	}
	r.out = bufio.NewWriter(refused{syscall.ENOSPC})
	r.Emit(Event{Type: "unit", Level: "info", Message: "lost"})
	r.Emit(Event{Type: "unit", Level: "info", Message: "stored"})
	if err := r.Finish(func(*Record) {}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	rec, err := Read(r.Dir)
	if err != nil || rec.Status != Finished || !rec.EventsIncomplete {
		t.Fatalf("record %+v %v", rec, err)
	}
	var stored []string
	if err := ReadRun(context.Background(), r.Dir, false, func(ev Event) { stored = append(stored, ev.Message) }); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0] != "stored" || !strings.HasPrefix(stored[1], "not every event of the run was stored: ") {
		t.Fatalf("stored %q", stored)
	}
	if last := got[len(got)-1]; last.Type != "diagnostic" || !strings.Contains(last.Message, syscall.ENOSPC.Error()) {
		t.Fatalf("the sink got %+v last", last)
	}
}

// A record that cannot be written keeps its lock file, so a later run
// marks it once the disk takes it: as its last event says, or as
// interrupted without that event.
func TestRecordNotWritten(t *testing.T) {
	root, newID := t.TempDir(), ids()
	for _, last := range []bool{true, false} {
		r, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "task", Host: "h", PID: 1, Started: "now"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		block := filepath.Join(r.Dir, ".record.tmp")
		if err := os.Mkdir(block, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := r.Finish(func(rec *Record) { rec.Outcome = "success" }); err == nil {
			t.Fatal("Finish wrote the record through the block")
		}
		if last {
			code := 1
			r.Emit(Event{Type: "run", Level: "info", Message: "success", Outcome: "success", ExitCode: &code,
				Outputs: map[string]any{"id": "x"}})
		}
		r.Close()
		if _, err := os.Stat(filepath.Join(r.Dir, ".lock")); err != nil {
			t.Fatalf("the lock file went: %v", err)
		}
		if got, err := Prune(root, "dev"); err == nil || len(got) != 0 {
			t.Fatalf("marked through the block: %v %v", got, err)
		}
		if err := os.Remove(block); err != nil {
			t.Fatal(err)
		}
		got, err := Prune(root, "dev")
		if err != nil || len(got) != 1 || got[0].Run != r.ID() {
			t.Fatalf("interrupted %v %v", got, err)
		}
		rec, err := Read(r.Dir)
		switch {
		case err != nil || rec.Status != Finished:
			t.Fatalf("record %+v %v", rec, err)
		case last && (rec.Outcome != "success" || rec.ExitCode != 1 || rec.Reason != "" || rec.Outputs["id"] != "x"):
			t.Fatalf("record from the last event %+v", rec)
		case !last && (rec.Outcome != "cancelled" || rec.Reason != "interrupted"):
			t.Fatalf("record without the last event %+v", rec)
		}
	}
}

// A record that says running without its lock file, which an older
// OpsTruss removed after it failed to write the record, is no live run.
func TestInterruptedWithoutLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "runs", "dev", "20261023T000000Z-00000001")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := write(dir, &Record{Version: Version, Run: filepath.Base(dir), Project: "shop", Stack: "dev", Command: "task",
		Host: "h", PID: 1, Started: "now", Status: Running}); err != nil {
		t.Fatal(err)
	}
	if got, err := Prune(root, "dev"); err != nil || len(got) != 1 || got[0].Reason != "interrupted" {
		t.Fatalf("interrupted %v %v", got, err)
	}
}

// Records and events of a newer version are refused, not read as these.
func TestNewerVersions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "20261023T000000Z-00000001")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"version":999,"status":"finished"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var newer *state.NewerError
	if _, err := Read(dir); !errors.As(err, &newer) {
		t.Fatalf("Read: %v", err)
	}
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte(`{"v":999,"seq":1,"type":"log","message":"future"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := false
	if err := ReadRun(context.Background(), dir, false, func(Event) { read = true }); !errors.As(err, &newer) || read {
		t.Fatalf("ReadRun: %v, read %v", err, read)
	}
	if err := ReadLog(context.Background(), events, false, func(Event) { read = true }); !errors.As(err, &newer) || read {
		t.Fatalf("ReadLog: %v, read %v", err, read)
	}
}

// A log that cannot be written loses events, and says how many with the
// first event it writes after; a file that a turn could not open is
// opened again.
func TestLogLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "_daemon.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(Event{Type: "daemon", Level: "info", Message: "one"})
	_ = l.f.Close()
	l.Emit(Event{Type: "daemon", Level: "info", Message: "two"})
	l.Emit(Event{Type: "daemon", Level: "info", Message: "three"})
	l.f = nil
	l.Emit(Event{Type: "daemon", Level: "info", Message: "four"})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ReadLog(context.Background(), path, false, func(ev Event) { got = append(got, fmt.Sprintf("%d %s", ev.Seq, ev.Message)) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "1 one" || !strings.HasPrefix(got[1], "2 2 events of this log were not stored: ") || got[2] != "3 four" {
		t.Fatalf("log %q", got)
	}
}

// A new process prunes from the index, without the records of the runs
// it lists; an index that is lost or broken is built again from the runs,
// and one whose lines are mostly of runs that went is written anew.
func TestIndex(t *testing.T) {
	root, newID := t.TempDir(), ids()
	base := filepath.Join(root, "runs", "dev")
	var first string
	for i := range Keep + 1 {
		r, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "task", Task: "backup", Host: "h", PID: 1, Started: "now"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = r.Dir
		}
		if err := r.Finish(func(*Record) {}); err != nil {
			t.Fatal(err)
		}
		r.Close()
		if i == Keep-1 {
			// The first Keep runs are in the index once a run read them.
			if _, err := Prune(root, "dev"); err != nil {
				t.Fatal(err)
			}
		}
	}
	newProcess := func() { indexes.Delete(base) }
	newProcess()
	// The record of the oldest run is gone, so only the index tells Prune
	// that it is a run of backup, the one beyond Keep.
	if err := os.Remove(filepath.Join(first, "record.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("Prune did not read the index: %v", err)
	}
	newProcess()
	if err := os.WriteFile(filepath.Join(base, ".index"), []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	newProcess()
	names, ix, err := scan(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.runs) != Keep || len(names) != Keep {
		t.Fatalf("index of %d runs, %d runs", len(ix.runs), len(names))
	}
	ix.mu.Unlock()
	// Lines of runs that went outnumber the runs: the file is written anew.
	var gone []byte
	for i := range 2*Keep + 100 {
		gone = fmt.Appendf(gone, `{"v":1,"run":"20000101T000000Z-%08x","command":"task","size":1}`+"\n", i)
	}
	f, err := os.OpenFile(filepath.Join(base, ".index"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(gone); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(base, ".index"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte{'\n'}); n != Keep || bytes.Contains(data, []byte("20000101T")) {
		t.Fatalf("the index has %d lines after compaction", n)
	}
}

// An index whose lock another process keeps, as one that was stopped
// holding it, does not hold up Prune: the line is not written, and a later
// process reads that record again.
func TestIndexHeld(t *testing.T) {
	root := t.TempDir()
	r, err := Create(root, ids(), &Record{Project: "shop", Stack: "dev", Command: "task", Task: "backup", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(func(*Record) {}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	path := filepath.Join(root, "runs", "dev", ".index")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Prune(root, "dev"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prune waits for the lock of the index")
	}
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatalf("the index got %q", data)
	}
}
