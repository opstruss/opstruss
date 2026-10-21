package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

func project(t *testing.T, yaml string) Request {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	return Request{Dir: dir, Environ: os.Environ()}
}

func TestApprove(t *testing.T) {
	req := project(t, "name: shop\nunits:\n  web:\n    run: sleep 0.2; echo up >> log\n    timeout: 1s\n")
	var asked []engine.Question
	answer := false
	a := New(Handlers{Approve: func(_ context.Context, q engine.Question) (bool, error) {
		asked = append(asked, q)
		// The answer takes longer than the unit's timeout, which does not
		// count it.
		time.Sleep(1200 * time.Millisecond)
		return answer, nil
	}})
	res, err := a.Up(context.Background(), req)
	if err != nil || res.ExitCode != 2 {
		t.Fatalf("no: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(req.Dir, "log")); err == nil {
		t.Fatal("the unit ran after no")
	}
	answer = true
	res, err = a.Up(context.Background(), req)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("yes: %+v %v", res, err)
	}
	if len(asked) != 2 || asked[1].Address != "default/web" || !strings.Contains(asked[1].Summary, "will run: new") {
		t.Fatalf("asked %+v", asked)
	}
	rec, err := record.Read(filepath.Join(req.Dir, ".opstruss", "runs", "default", res.Run))
	if err != nil || rec.Units["web"] == nil || rec.Units["web"].Approval != "terminal" {
		t.Fatalf("record %+v %v", rec, err)
	}
}

func TestAsk(t *testing.T) {
	req := project(t, `name: shop
tasks:
  greet:
    inputs:
      who: {type: string, required: true}
    run: echo "msg=hi $INPUT_WHO" >> "$OPSTRUSS_OUTPUT"
    outputs:
      msg: {}
`)
	var last record.Event
	a := New(Handlers{Event: func(ev record.Event) { last = ev },
		Ask: func(_ context.Context, name string, in *model.Input) (string, error) { return "Ana", nil }})
	req.Task = "greet"
	res, err := a.Run(context.Background(), req)
	if err != nil || res.ExitCode != 0 || last.Type != "run" || last.Outputs["msg"] != "hi Ana" {
		t.Fatalf("%+v %v %+v", res, err, last)
	}
}

// TestCancelInProcess makes two calls of a task with concurrency: cancel in
// one process, as the runs of a daemon are: the second stops the first,
// and no signal reaches the process.
func TestCancelInProcess(t *testing.T) {
	req := project(t, `name: shop
tasks:
  hold:
    concurrency: cancel
    inputs:
      seconds: {type: number}
    run: touch started; sleep "$INPUT_SECONDS"
`)
	a := New(Handlers{})
	first := make(chan *Result)
	go func() {
		r := req
		r.Task, r.With = "hold", map[string]any{"seconds": 30.0}
		res, err := a.Run(context.Background(), r)
		if err != nil {
			t.Error(err)
		}
		first <- res
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(req.Dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first call did not start")
		}
	}
	r := req
	r.Task, r.With = "hold", map[string]any{"seconds": 0.0}
	began := time.Now()
	res, err := a.Run(context.Background(), r)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("second call: %+v %v", res, err)
	}
	if res := <-first; res == nil || res.ExitCode == 0 || time.Since(began) > 20*time.Second {
		t.Fatalf("first call: %+v after %v", res, time.Since(began))
	}
}
