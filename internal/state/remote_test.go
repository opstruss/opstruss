package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// memServer answers the backend protocol from memory, as a plugin backend
// does on its stdin and stdout; deleteAny breaks the contract as storage
// does that ignores If-Match on delete.
type memServer struct {
	mu        sync.Mutex
	keys      map[string]string
	locks     map[string][2]string // holder JSON, token
	deleteAny bool
	protocol  int
	silent    string // a method it never answers
	deaf      bool   // reads no request after initialize
	n         int
}

func (m *memServer) serve(t *testing.T, in io.Reader, out io.WriteCloser) {
	defer func() { _ = out.Close() }()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			t.Errorf("request: %v", err)
			return
		}
		if req.Method == m.silent {
			continue
		}
		res, code := m.answer(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if code != "" {
			e := map[string]any{"code": -32000, "message": code + " " + req.Method}
			if code != "other" {
				e["data"] = map[string]any{"code": code}
			}
			if code == "locked" {
				key, _ := req.Params["key"].(string)
				var h any
				_ = json.Unmarshal([]byte(m.locks[key][0]), &h)
				e["data"] = map[string]any{"code": code, "holder": h, "token": m.locks[key][1]}
			}
			resp["error"] = e
		} else {
			resp["result"] = res
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
		if m.deaf {
			select {}
		}
	}
}

func (m *memServer) answer(method string, p map[string]any) (any, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	str := func(k string) string { s, _ := p[k].(string); return s }
	key := str("key")
	v, ok := m.keys[key]
	switch method {
	case "initialize":
		return map[string]any{"protocol": m.protocol, "location": "mem://test/", "capabilities": map[string]any{}}, ""
	case "get":
		if !ok {
			return nil, "not_found"
		}
		return map[string]any{"data": v, "etag": ETag([]byte(v))}, ""
	case "put":
		if none, _ := p["if_none_match"].(bool); none && ok || str("if_match") != "" && (!ok || ETag([]byte(v)) != str("if_match")) {
			return nil, "conflict"
		}
		m.keys[key] = str("data")
		return map[string]any{"etag": ETag([]byte(str("data")))}, ""
	case "delete":
		switch {
		case !ok:
			return nil, "not_found"
		case ETag([]byte(v)) != str("if_match") && !m.deleteAny:
			return nil, "conflict"
		}
		delete(m.keys, key)
		return map[string]any{}, ""
	case "list":
		keys := []string{}
		for k := range m.keys {
			if strings.HasPrefix(k, str("prefix")) {
				keys = append(keys, k)
			}
		}
		for k := range m.locks {
			if strings.HasPrefix(k, str("prefix")) {
				keys = append(keys, k)
			}
		}
		return map[string]any{"keys": keys}, ""
	case "lock":
		if _, held := m.locks[key]; held {
			return nil, "locked"
		}
		m.n++
		h, _ := json.Marshal(p["holder"])
		token := "token-" + string(rune('a'+m.n))
		m.locks[key] = [2]string{string(h), token}
		return map[string]any{"token": token}, ""
	case "unlock":
		l, held := m.locks[key]
		switch {
		case !held:
			return nil, "not_found"
		case l[1] != str("token"):
			return nil, "conflict"
		}
		delete(m.locks, key)
		return map[string]any{}, ""
	}
	return nil, "other"
}

// dial connects a Remote to m through pipes.
func dial(t *testing.T, m *memServer, timeout time.Duration) (*Remote, error) {
	t.Helper()
	if m.keys == nil {
		m.keys, m.locks = map[string]string{}, map[string][2]string{}
	}
	if m.protocol == 0 {
		m.protocol = BackendProtocol
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go m.serve(t, inR, outW)
	stopped := make(chan struct{})
	r := &Remote{In: inW, Out: outR, Timeout: timeout, Stop: func() { close(stopped); _ = outW.Close() }}
	t.Cleanup(func() { r.Close(); _ = inW.Close() })
	return r, r.Open()
}

func TestRemote(t *testing.T) {
	m := &memServer{}
	r, err := dial(t, m, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.Location != "mem://test/" {
		t.Fatalf("location %q", r.Location)
	}
	if _, _, err := r.Get("prod/_meta"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("get of a missing key: %v", err)
	}
	etag, err := r.Put("prod/web/state.json", []byte(`{"version":1}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Put("prod/web/state.json", []byte(`{}`), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("put of a key that exists: %v", err)
	}
	if _, err := r.Put("prod/web/state.json", []byte(`{}`), "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("put with another etag: %v", err)
	}
	data, got, err := r.Get("prod/web/state.json")
	if err != nil || string(data) != `{"version":1}` || got != etag {
		t.Fatalf("get: %q %q %v", data, got, err)
	}
	h := Holder{Run: "r1", Host: "h1", PID: 7}
	token, err := r.Lock("prod/web/state.lock", h)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Lock("prod/web/state.lock", Holder{Run: "r2"})
	var le *LockedError
	if !errors.As(err, &le) || le.Holder.Run != "r1" || le.Token != token {
		t.Fatalf("second lock: %v", err)
	}
	if err := r.Unlock("prod/web/state.lock", "other"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("unlock with another token: %v", err)
	}
	if err := r.Unlock("prod/web/state.lock", token); err != nil {
		t.Fatal(err)
	}
	if err := r.Unlock("prod/web/state.lock", token); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("unlock of a lock that is gone: %v", err)
	}
	keys, err := r.List("prod/")
	if err != nil || len(keys) != 1 || keys[0] != "prod/web/state.json" {
		t.Fatalf("list: %v %v", keys, err)
	}
	if err := r.Delete("prod/web/state.json", etag); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("prod/web/state.json", etag); !errors.Is(err, ErrNotExist) {
		t.Fatalf("delete of a missing key: %v", err)
	}
	// An error the contract does not name fails the operation only.
	m.silent = ""
	if err := r.call("other", nil, nil); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("unknown method: %v", err)
	}
	if _, _, err := r.Get("prod/_meta"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("after an error of the backend: %v", err)
	}
}

func TestRemoteBroken(t *testing.T) {
	if _, err := dial(t, &memServer{protocol: 2}, time.Minute); err == nil || !strings.Contains(err.Error(), "speaks protocol 2") {
		t.Fatalf("another protocol: %v", err)
	}
	// An answer that never comes stops the program, and every operation
	// after it fails with why.
	m := &memServer{}
	r, err := dial(t, m, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	m.silent = "get"
	if _, _, err := r.Get("prod/_meta"); err == nil || !strings.Contains(err.Error(), "did not answer get within 50ms") {
		t.Fatalf("no answer: %v", err)
	}
	if _, err := r.List("prod/"); err == nil || !strings.Contains(err.Error(), "did not answer get") {
		t.Fatalf("after no answer: %v", err)
	}
	if r.Broken() == nil {
		t.Fatal("not broken")
	}
	// A program that reads no more does not hold a request either.
	r, err = dial(t, &memServer{deaf: true}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Put("prod/web/state.json", []byte("{}"), ""); err == nil || !strings.Contains(err.Error(), "did not read put within 50ms") {
		t.Fatalf("a program that reads nothing: %v", err)
	}
}

func TestProbe(t *testing.T) {
	r, err := dial(t, &memServer{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(r, Holder{Run: "r1"}); err != nil {
		t.Fatal(err)
	}
	if keys, _ := r.List("_probe/"); len(keys) > 0 {
		t.Fatalf("the probe left %v", keys)
	}
	r, err = dial(t, &memServer{deleteAny: true}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	err = Probe(r, Holder{Run: "r1"})
	var pe *ProbeError
	if !errors.As(err, &pe) || pe.Step != 2 || !strings.Contains(err.Error(), "delete with another etag is a conflict") {
		t.Fatalf("a storage that deletes whatever etag: %v", err)
	}
	if keys, _ := r.List("_probe/"); len(keys) > 0 {
		t.Fatalf("the failed probe left %v", keys)
	}
}
