package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	// A crash lets go of the lock without finishing the record.
	_ = r.lock.Close()
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
