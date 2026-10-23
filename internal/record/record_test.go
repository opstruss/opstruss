package record

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/schema"
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
	code := 0
	r.Emit(Event{Type: "run", Level: "info", Message: "success", Outcome: "success", ExitCode: &code})
	if err := r.Finish(func(rec *Record) {
		rec.Outcome, rec.Units = "success", map[string]*Unit{"web": {Outcome: "success", Changed: true}}
		rec.Tasks = map[string]*Task{"build": {Outcome: "success"}}
	}); err != nil {
		t.Fatal(err)
	}
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
	if got, _ := Interrupted(root, "dev"); len(got) != 0 {
		t.Fatalf("a live run is interrupted: %v", got)
	}
	// A crash lets go of the lock without finishing the record, and the
	// process that knew the run is gone.
	_ = r.lock.Close()
	live.Delete(r.ID())
	got, err := Interrupted(root, "dev")
	if err != nil || len(got) != 1 || got[0] != r.ID() {
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
	}
	running, err := Create(root, newID, &Record{Project: "shop", Stack: "dev", Command: "task", Task: "backup", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Prune(root, "dev"); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "runs", "dev"))
	// The newest 100 of backup, the running one among them, the first
	// backup run, the last to touch db, and the run of up.
	if len(ents) != Keep+2 {
		t.Fatalf("%d runs are left", len(ents))
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
	if got, err := Interrupted(root, "dev"); err != nil || len(got) != 1 {
		t.Fatalf("interrupted %v %v", got, err)
	}
	if err := Prune(root, "dev"); err != nil {
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
	select {
	case lines := <-got:
		if strings.Join(lines, ",") != "first,second" {
			t.Fatalf("followed %v", lines)
		}
	case <-ctx.Done():
		t.Fatal("ReadRun did not end with the run")
	}
}
