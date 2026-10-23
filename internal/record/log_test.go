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

// ReadLog reads the files a log turned over into first, and with follow
// goes on across turns, each event once and in order.
func TestReadLogFollow(t *testing.T) {
	maxBefore, every := logMax, followEvery
	logMax, followEvery = 400, 5*time.Millisecond
	t.Cleanup(func() { logMax, followEvery = maxBefore, every })
	p := filepath.Join(t.TempDir(), "web.jsonl")
	l, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	emit := func(from, to int) {
		for i := from; i < to; i++ {
			l.Emit(Event{Type: "log", Level: "info", Address: "default/web", Message: fmt.Sprintf("line %d", i)})
		}
	}
	emit(0, 3)

	var got []string
	if err := ReadLog(context.Background(), p, false, func(ev Event) { got = append(got, ev.Message) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "line 0,line 1,line 2" {
		t.Fatalf("read %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 100)
	done := make(chan error, 1)
	go func() { done <- ReadLog(ctx, p, true, func(ev Event) { lines <- ev.Message }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// Enough to turn the log over more than once, but not so often that a
	// file is gone before it is read.
	for i := 3; i < 12; i++ {
		emit(i, i+1)
		time.Sleep(20 * time.Millisecond)
	}
	var seen []string
	for deadline := time.After(5 * time.Second); len(seen) < 12; {
		select {
		case s := <-lines:
			seen = append(seen, s)
		case <-deadline:
			t.Fatalf("followed %v", seen)
		}
	}
	for i, s := range seen {
		if want := fmt.Sprintf("line %d", i); s != want {
			t.Fatalf("followed %v", seen)
		}
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("the log did not turn over: %v", err)
	}
}

// A log that turns over twice while ReadLog waits is still followed whole:
// the file in between is read before the new one, as tail -F reads it.
func TestReadLogFollowTwoTurns(t *testing.T) {
	maxBefore, every := logMax, followEvery
	logMax, followEvery = 400, 300*time.Millisecond
	t.Cleanup(func() { logMax, followEvery = maxBefore, every })
	p := filepath.Join(t.TempDir(), "web.jsonl")
	l, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	emit := func(from, to int) {
		for i := from; i < to; i++ {
			l.Emit(Event{Type: "log", Level: "info", Address: "default/web", Message: fmt.Sprintf("line %d", i)})
		}
	}
	emit(0, 1)

	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 100)
	done := make(chan error, 1)
	go func() { done <- ReadLog(ctx, p, true, func(ev Event) { lines <- ev.Message }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("line 0 was not read")
	}
	// ReadLog waits for its next look now; the log turns over twice before.
	time.Sleep(50 * time.Millisecond)
	emit(1, 8)
	if _, err := os.Stat(p + ".2"); err != nil {
		t.Fatalf("the log did not turn over twice: %v", err)
	}
	seen := []string{"line 0"}
	for deadline := time.After(5 * time.Second); len(seen) < 8; {
		select {
		case s := <-lines:
			seen = append(seen, s)
		case <-deadline:
			t.Fatalf("followed %v", seen)
		}
	}
	for i, s := range seen {
		if want := fmt.Sprintf("line %d", i); s != want {
			t.Fatalf("followed %v", seen)
		}
	}
}
