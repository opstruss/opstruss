package builtin

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/model"
)

// call conforms inputs to the schema of task, as the engine does, and runs
// it.
func call(t *testing.T, task, dir string, given map[string]any) *Result {
	t.Helper()
	in := map[string]any{}
	for k, decl := range model.Builtin(task).Inputs {
		v, err := decl.Conform(given[k])
		if err != nil {
			t.Fatalf("%s.%s: %v", task, k, err)
		}
		in[k] = v
	}
	if task == "set" {
		in = given
	}
	var logged []string
	res := Run(context.Background(), &Call{Task: task, Inputs: in, Dir: dir, Log: func(level, msg string) {
		logged = append(logged, level+" "+msg)
	}})
	if task == "log" {
		res.Outputs["logged"] = strings.Join(logged, "\n")
	}
	return res
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-Request", r.Header.Get("X-Token")+" "+r.Header.Get("Content-Type"))
			_, _ = w.Write([]byte(`{"got": ` + string(body) + `, "n": 2}`))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("no such page"))
		}
	}))
	defer srv.Close()

	res := call(t, "http", "", map[string]any{"url": srv.URL + "/json", "method": "post",
		"headers": map[string]any{"X-Token": "t1"}, "body": map[string]any{"a": int64(1)}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	body := res.Outputs["body"].(map[string]any)
	if body["n"] != int64(2) || body["got"].(map[string]any)["a"] != int64(1) {
		t.Errorf("body = %v", body)
	}
	if h := res.Outputs["headers"].(map[string]any)["x-request"]; h != "t1 application/json" {
		t.Errorf("x-request = %v", h)
	}
	if !res.Changed || res.Outputs["status"] != int64(200) || res.Outputs["tls"] != nil {
		t.Errorf("changed %v, status %v, tls %v", res.Changed, res.Outputs["status"], res.Outputs["tls"])
	}

	// a status that status_code does not accept fails, with the outputs kept
	res = call(t, "http", "", map[string]any{"url": srv.URL + "/missing"})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "answered 404") || res.Outputs["body"] != "no such page" || res.Changed {
		t.Errorf("err %v, outputs %v, changed %v", res.Err, res.Outputs, res.Changed)
	}
	for _, codes := range []any{"4xx", int64(404), []any{"2xx", "404"}} {
		if res := call(t, "http", "", map[string]any{"url": srv.URL + "/missing", "status_code": codes}); res.Err != nil {
			t.Errorf("status_code %v: %v", codes, res.Err)
		}
	}
	if res := call(t, "http", "", map[string]any{"url": srv.URL + "/missing", "status_code": "4x"}); res.Err == nil {
		t.Error("status_code 4x: no error")
	}
}

func TestHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// The server's certificate is trusted through ca only.
	if res := call(t, "http", "", map[string]any{"url": srv.URL}); res.Err == nil {
		t.Fatal("a certificate that the system does not trust was accepted")
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	res := call(t, "http", "", map[string]any{"url": srv.URL, "ca": ca})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	tls := res.Outputs["tls"].(map[string]any)
	certs := tls["certificates"].([]any)
	leaf := certs[0].(map[string]any)
	if !strings.HasPrefix(tls["version"].(string), "TLS 1.") || leaf["not_after"] == "" || len(leaf["dns_names"].([]any)) == 0 {
		t.Errorf("tls = %v", tls)
	}
	if _, err := time.Parse(time.RFC3339, leaf["not_after"].(string)); err != nil {
		t.Error(err)
	}
	if res := call(t, "http", "", map[string]any{"url": srv.URL, "ca": "not a certificate"}); res.Err == nil {
		t.Error("ca without a certificate: no error")
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(mode any) *Result {
		return call(t, "write", dir, map[string]any{"path": "conf.json", "content": map[string]any{"b": int64(2), "a": int64(1)},
			"format": "json", "mode": mode})
	}
	if res := write(nil); res.Err != nil || !res.Changed || res.Outputs["path"] != filepath.Join(dir, "conf.json") {
		t.Fatalf("first write: %v, changed %v, %v", res.Err, res.Changed, res.Outputs)
	}
	if res := write(nil); res.Err != nil || res.Changed {
		t.Errorf("same content: %v, changed %v", res.Err, res.Changed)
	}
	if res := write("0600"); res.Err != nil || !res.Changed {
		t.Errorf("new mode: %v, changed %v", res.Err, res.Changed)
	}
	info, err := os.Stat(filepath.Join(dir, "conf.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
	if res := write(nil); res.Changed {
		t.Error("a write without mode changed the mode back")
	}
	res := call(t, "read", dir, map[string]any{"path": "conf.json", "format": "json"})
	if res.Err != nil || res.Outputs["content"].(map[string]any)["b"] != int64(2) {
		t.Errorf("read: %v, %v", res.Err, res.Outputs)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %d files, want 1", len(entries))
	}
	if res := call(t, "write", dir, map[string]any{"path": "nope/x.txt", "content": "x"}); res.Err == nil {
		t.Error("a write into a missing directory: no error")
	}
}

func TestSetAssertLog(t *testing.T) {
	if res := call(t, "set", "", map[string]any{"a": []any{int64(1)}}); res.Outputs["a"].([]any)[0] != int64(1) {
		t.Errorf("set: %v", res.Outputs)
	}
	for _, tt := range []struct {
		that any
		msg  any
		want string
	}{
		{true, nil, ""},
		{[]any{true, true}, nil, ""},
		{false, "the disk is full", "the disk is full"},
		{[]any{true, false}, nil, "item 1 of that is false"},
		{"yes", nil, "that takes conditions"},
	} {
		res := call(t, "assert", "", map[string]any{"that": tt.that, "msg": tt.msg})
		if got := errText(res.Err); !strings.Contains(got, tt.want) || (tt.want == "") != (res.Err == nil) {
			t.Errorf("assert %v: %q, want %q", tt.that, got, tt.want)
		}
	}
	if res := call(t, "log", "", map[string]any{"msg": map[string]any{"a": int64(1)}}); res.Outputs["logged"] != `info {"a":1}` {
		t.Errorf("log: %q", res.Outputs["logged"])
	}
}

func TestWait(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if res := call(t, "wait", "", map[string]any{"tcp": l.Addr().String(), "timeout": "5s"}); res.Err != nil {
		t.Error(res.Err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	if res := call(t, "wait", "", map[string]any{"tcp": addr, "timeout": "1500ms"}); res.Err == nil {
		t.Error("a closed port was taken as open")
	}
	if res := call(t, "wait", "", map[string]any{"seconds": int64(0)}); res.Err != nil {
		t.Error(res.Err)
	}
	if res := call(t, "wait", "", map[string]any{"seconds": int64(1), "tcp": addr}); res.Err == nil {
		t.Error("two things to wait for: no error")
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
