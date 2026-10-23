package record

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A request reaches the run whose directory it is in, while that run
// lives; a line still being written waits for its end, and a run that
// ended takes none.
func TestCancelRequests(t *testing.T) {
	root := t.TempDir()
	r, err := Create(root, ids(), &Record{Project: "shop", Stack: "dev", Command: "task", Task: "hold", Host: "h", PID: 1, Started: "now"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Cancel, 4)
	r.WatchCancel(func(c Cancel) { got <- c })
	path := filepath.Join(r.Dir, cancelFile)
	if err := os.WriteFile(path, []byte(`{"reason":"requested","us`), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		t.Fatalf("a part of a line was read: %+v", c)
	case <-time.After(3 * flushEvery):
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("er\":\"a\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := RequestCancel(r.Dir, Cancel{Reason: "requested", Force: true, User: "b"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []Cancel{{Reason: "requested", User: "a"}, {Reason: "requested", Force: true, User: "b"}} {
		select {
		case c := <-got:
			if c.Reason != want.Reason || c.Force != want.Force || c.User != want.User {
				t.Fatalf("got %+v, want %+v", c, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no request %+v", want)
		}
	}
	if err := r.Finish(func(*Record) {}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := RequestCancel(r.Dir, Cancel{Reason: "requested"}); !errors.Is(err, ErrEnded) {
		t.Fatalf("a request for a run that ended: %v", err)
	}
}
