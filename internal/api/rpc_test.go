package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// serve listens on a socket in a short directory, since the path of a
// Unix socket is short, and serves each connection with s.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.ServeConn(context.Background(), nc)
		}
	}()
	return p
}

func TestSocketCall(t *testing.T) {
	var got Request
	p := serve(t, &Server{Version: "v1", Call: func(ctx context.Context, method string, r Request, h Handlers) (*Result, error) {
		got = r
		h.Event(record.Event{Type: "log", Level: "info", Message: "hello"})
		ok, err := h.Approve(ctx, engine.Question{Address: "prod/web", Summary: "will run"})
		if err != nil || !ok {
			return nil, errors.New("not approved")
		}
		name, err := h.Ask(ctx, "who", &model.Input{Description: "a name", Sensitive: true})
		if err != nil || name != "Ana" {
			return nil, errors.New("no name")
		}
		return &Result{Run: "r1", ExitCode: 2}, nil
	}})
	c, err := Dial(p, "v1", true)
	if err != nil || c.Version != "v1" {
		t.Fatalf("dial %v %v", c, err)
	}
	var events []string
	var asked []string
	res, err := c.Call(context.Background(), "up", Request{Dir: "/p", Units: []string{"web"}, Environ: []string{"A=1"}}, Handlers{
		Event: func(ev record.Event) { events = append(events, ev.Message) },
		Approve: func(_ context.Context, q engine.Question) (bool, error) {
			asked = append(asked, q.Address+": "+q.Summary)
			return true, nil
		},
		Ask: func(_ context.Context, name string, in *model.Input) (string, error) {
			asked = append(asked, name+" ("+in.Description+")")
			return "Ana", nil
		},
	})
	if err != nil || res.Run != "r1" || res.ExitCode != 2 {
		t.Fatalf("call %+v %v", res, err)
	}
	if got.Dir != "/p" || got.Units[0] != "web" || got.Environ[0] != "A=1" {
		t.Fatalf("request %+v", got)
	}
	if strings.Join(events, ",") != "hello" || strings.Join(asked, ",") != "prod/web: will run,who (a name)" {
		t.Fatalf("events %q, asked %q", events, asked)
	}
}

func TestSocketCancelAndGone(t *testing.T) {
	gone := make(chan bool, 1)
	p := serve(t, &Server{Version: "v1", Call: func(ctx context.Context, method string, r Request, h Handlers) (*Result, error) {
		if method == "run" {
			// The caller goes away while it would answer.
			ok, err := h.Approve(ctx, engine.Question{Summary: "will run"})
			gone <- !ok && err == nil
			return &Result{Run: "r2"}, nil
		}
		h.Event(record.Event{Type: "run", Level: "info", Message: "started"})
		<-ctx.Done()
		return &Result{Run: "r1", ExitCode: 1}, nil
	}})
	c, err := Dial(p, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res, err := c.Call(ctx, "up", Request{Dir: "/p"}, Handlers{Event: func(record.Event) { cancel() }})
	if err != nil || res.ExitCode != 1 {
		t.Fatalf("cancelled call %+v %v", res, err)
	}
	// A caller with a terminal that is gone answers as one without.
	nc, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	w := json.NewEncoder(nc)
	_ = w.Encode(message{JSONRPC: "2.0", ID: raw(0), Method: "initialize", Params: raw(Initialize{Protocol: Protocol, Version: "v1", Terminal: true})})
	_ = w.Encode(message{JSONRPC: "2.0", ID: raw(1), Method: "run", Params: raw(Request{Dir: "/p", Task: "t"})})
	r := bufio.NewReader(nc)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(line, []byte(`"method":"approve"`)) {
			_ = nc.Close()
			break
		}
	}
	select {
	case ok := <-gone:
		if !ok {
			t.Fatal("approve did not answer no")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approve waited for a caller that is gone")
	}
}

func TestSocketRefuses(t *testing.T) {
	var mu sync.Mutex
	stopping := false
	p := serve(t, &Server{Version: "v1",
		Stopping: func() bool { mu.Lock(); defer mu.Unlock(); return stopping },
		Call: func(context.Context, string, Request, Handlers) (*Result, error) {
			return nil, model.Problems{{File: "opstruss.yaml", Line: 2, Column: 3, Message: "bad"}}
		}})
	call := func(method string, params any) error {
		c, err := Dial(p, "v1", false)
		if err != nil {
			return err
		}
		_ = c.c.send(message{ID: raw(1), Method: method, Params: raw(params)})
		m, err := c.c.read(time.Second)
		if err != nil {
			return err
		}
		return callError(m.Error)
	}
	var probs model.Problems
	if err := call("up", Request{Dir: "/p"}); !errors.As(err, &probs) || probs[0].Line != 2 {
		t.Fatalf("problems: %v", err)
	}
	if err := call("exec", Request{Dir: "/p"}); err == nil || !strings.Contains(err.Error(), `no method "exec"`) {
		t.Fatalf("method: %v", err)
	}
	if err := call("up", map[string]any{"dir": "/p", "unknown": 1}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("params: %v", err)
	}
	// A message larger than MaxMessage ends the connection without a call.
	if err := call("up", Request{Dir: "/p", Environ: []string{strings.Repeat("x", MaxMessage)}}); err == nil || errors.As(err, new(model.Problems)) {
		t.Fatalf("a message larger than MaxMessage was read: %v", err)
	}
	mu.Lock()
	stopping = true
	mu.Unlock()
	if err := call("up", Request{Dir: "/p"}); !errors.Is(err, ErrStopping) {
		t.Fatalf("stopping: %v", err)
	}
	// Another protocol is refused before a call.
	nc, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	c := newConn(nc)
	_ = c.send(message{ID: raw(0), Method: "initialize", Params: raw(Initialize{Protocol: Protocol + 1, Version: "v2"})})
	m, err := c.read(time.Second)
	if err != nil || m.Error == nil || m.Error.Code != codeProtocol || m.Error.Data.Protocol != Protocol || m.Error.Data.Version != "v1" {
		t.Fatalf("protocol: %+v %v", m, err)
	}
}

func TestOpenProtocolError(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		c := newConn(b)
		m, _ := c.read(0)
		_ = c.send(message{ID: m.ID, Error: &rpcError{Code: codeProtocol, Message: "no", Data: &errorData{Protocol: 7, Version: "v7"}}})
	}()
	_, err := open(newConn(a), "v1", false)
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Protocol != 7 || pe.Version != "v7" {
		t.Fatalf("got %v", err)
	}
}

// TestSchemas keeps api.json in step with what the socket sends.
func TestSchemas(t *testing.T) {
	ev := record.Event{V: 1, Seq: 1, Time: "2026-10-22T00:00:00.000Z", Type: "log", Level: "info", Message: "m"}
	for kind, v := range map[schema.Kind]any{
		schema.APIInitialize: Initialize{Protocol: 1, Version: "v", Terminal: true},
		schema.APIRequest: Request{Dir: "/p", Cwd: "/", Stack: "s", Inputs: map[string]string{"a": "b"}, Environ: []string{"A=1"},
			Units: []string{"u"}, Yes: true, Force: true, NoDeps: true, Task: "t", With: map[string]any{"x": 1}, Run: "r",
			Debug: true, ShowSensitive: true},
		schema.APIResult:   Result{Run: "r", ExitCode: 1},
		schema.APIQuestion: engine.Question{Address: "s/u", Task: "t", Summary: "will run"},
		schema.APIApproval: approval{Approved: true},
		schema.APIAsk:      askParams{Name: "n", Description: "d", Sensitive: true},
		schema.APIAnswer:   answer{Value: "v"},
		schema.APIEvent:    ev,
	} {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw(v)))
		if err != nil {
			t.Fatal(err)
		}
		if probs, err := schema.Validate(kind, doc); err != nil || len(probs) > 0 {
			t.Errorf("%s: %v %v", kind, probs, err)
		}
	}
}
