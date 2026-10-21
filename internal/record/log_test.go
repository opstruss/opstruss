package record

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLog(t *testing.T, p string) []Event {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("%s: %v", sc.Text(), err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "_daemon.jsonl")
	l, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	l.Emit(Event{Type: "daemon", Level: "info", Message: "one"})
	l.Emit(Event{Type: "daemon", Level: "info", Message: "two"})
	_ = l.Close()
	// A crash cut the last line short.
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString(`{"v":1,"seq":3,"ti`)
	_ = f.Close()
	if l, err = OpenLog(p); err != nil {
		t.Fatal(err)
	}
	l.Emit(Event{Type: "daemon", Level: "info", Message: "three"})
	_ = l.Close()
	evs := readLog(t, p)
	if len(evs) != 3 || evs[2].Seq != 3 || evs[2].Message != "three" || evs[0].V != EventVersion || evs[0].Time == "" {
		t.Fatalf("events %+v", evs)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestLogTurns(t *testing.T) {
	logMax = 300
	defer func() { logMax = MaxLog }()
	p := filepath.Join(t.TempDir(), "x.jsonl")
	l, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		l.Emit(Event{Type: "log", Level: "info", Message: strings.Repeat("x", 50) + string(rune('a'+i))})
	}
	_ = l.Close()
	files, _ := filepath.Glob(p + "*")
	if len(files) != LogFiles {
		t.Fatalf("files %v", files)
	}
	last := readLog(t, p)
	older := readLog(t, p+".1")
	if last[len(last)-1].Message[50] != 't' || last[0].Seq != 1 || older[len(older)-1].Seq != int64(len(older)) {
		t.Fatalf("last %+v, older %+v", last, older)
	}
	for _, f := range files {
		if info, _ := os.Stat(f); info.Size() > logMax {
			t.Fatalf("%s has %d bytes", f, info.Size())
		}
	}
}
