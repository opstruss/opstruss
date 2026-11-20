package state

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// memServer answers the backend protocol from memory, as a plugin backend
// does on its stdin and stdout. Each flag breaks the contract as some
// storage does: deleteAny ignores If-Match on delete, matchNone refuses
// every put with If-Match, listNone lists nothing, and flatList lists only
// the keys right under the prefix, as a listing with a delimiter does. With
// scan it has the capability scan.
type memServer struct {
	mu           sync.Mutex
	keys         map[string]string
	locks        map[string][2]string // holder JSON, token
	deleteAny    bool
	matchNone    bool
	listNone     bool
	flatList     bool
	lockUnlisted bool // list and scan leave out the locks
	hashToken    bool // a lock's token is the hash of its holder
	scan         bool
	scanNone     bool // scan gives nothing
	scanNoLocks  bool // scan leaves out the locks
	protocol     int
	silent       string // a method it never answers
	deaf         bool   // reads no request after initialize
	n            int
	calls        []string // each method but initialize, with its key or prefix
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
	if method != "initialize" {
		m.calls = append(m.calls, method+" "+key+str("prefix"))
	}
	listed := func(k string) bool {
		rest, under := strings.CutPrefix(k, str("prefix"))
		return under && !m.listNone && (!m.flatList || !strings.Contains(rest, "/"))
	}
	switch method {
	case "initialize":
		return map[string]any{"protocol": m.protocol, "location": "mem://test/", "capabilities": map[string]any{"scan": m.scan}}, ""
	case "get":
		if !ok {
			return nil, "not_found"
		}
		return map[string]any{"data": v, "etag": ETag([]byte(v))}, ""
	case "put":
		if none, _ := p["if_none_match"].(bool); none && ok || str("if_match") != "" && (!ok || ETag([]byte(v)) != str("if_match") || m.matchNone) {
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
			if listed(k) {
				keys = append(keys, k)
			}
		}
		for k := range m.locks {
			if listed(k) && !m.lockUnlisted {
				keys = append(keys, k)
			}
		}
		return map[string]any{"keys": keys}, ""
	case "scan":
		items := []map[string]any{}
		for k, v := range m.keys {
			if listed(k) && !m.scanNone {
				items = append(items, map[string]any{"key": k, "data": v})
			}
		}
		for k, l := range m.locks {
			if listed(k) && !m.scanNone && !m.lockUnlisted && !m.scanNoLocks {
				items = append(items, map[string]any{"key": k, "data": l[0]})
			}
		}
		return map[string]any{"items": items}, ""
	case "lock":
		if _, held := m.locks[key]; held {
			return nil, "locked"
		}
		m.n++
		h, _ := json.Marshal(p["holder"])
		token := "token-" + string(rune('a'+m.n))
		if m.hashToken {
			token = fmt.Sprintf("%x", sha256.Sum256(h))
		}
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
	for _, c := range []struct {
		name string
		m    *memServer
		step int
		what string // in the error of the step; none passes
	}{
		{"a storage that keeps the contract", &memServer{}, 0, ""},
		{"a storage that deletes whatever etag", &memServer{deleteAny: true}, 2, "delete with an old etag is a conflict: it succeeded"},
		{"a storage that refuses every put with an etag", &memServer{matchNone: true}, 2, "put with the etag: the key changed since it was read"},
		{"a storage that lists nothing", &memServer{listNone: true}, 1, "list of the prefix gives the key"},
		{"a storage that lists one level", &memServer{flatList: true}, 1, "list of the prefix gives the key"},
		{"a storage whose token is the hash of the lock", &memServer{hashToken: true}, 5, "a lock after unlock has a token of its own"},
		{"a storage that lists no locks", &memServer{lockUnlisted: true}, 4, "list of the prefix gives the key _probe/"},
		{"a storage that keeps the contract, with scan", &memServer{scan: true}, 0, ""},
		{"a storage whose scan gives nothing", &memServer{scan: true, scanNone: true}, 1, "scan of the prefix gives the key"},
		{"a storage whose scan leaves out the locks", &memServer{scan: true, scanNoLocks: true}, 4, "scan of the prefix gives the key _probe/"},
	} {
		r, err := dial(t, c.m, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		err = Probe(r, Holder{Run: "r1"})
		var pe *ProbeError
		switch {
		case c.step == 0 && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.step != 0 && (!errors.As(err, &pe) || pe.Step != c.step || !strings.Contains(err.Error(), c.what)):
			t.Errorf("%s: %v, not step %d: %s", c.name, err, c.step, c.what)
		}
		c.m.mu.Lock()
		if len(c.m.keys)+len(c.m.locks) > 0 {
			t.Errorf("%s: the probe left %v and the locks %v", c.name, c.m.keys, c.m.locks)
		}
		c.m.mu.Unlock()
	}
}

// TestSnapshot reads a scope at once: scan, or list with a get of each
// record that is wanted, and locks seen in what was read, so a lock that
// is not there costs no operation.
func TestSnapshot(t *testing.T) {
	for _, scan := range []bool{true, false} {
		m := &memServer{scan: scan}
		r, err := dial(t, m, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		s := &Store{Backend: r, Stack: "prod", Project: "shop"}
		for _, a := range []string{"prod/web", "prod/db"} {
			if _, err := s.Put(a, &Record{ID: a}, ""); err != nil {
				t.Fatal(err)
			}
		}
		other := Holder{Run: "r2", Host: "h2", PID: 9}
		if _, err := r.Lock(LockKey("prod/db"), other); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		m.calls = nil
		m.mu.Unlock()
		sn, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if got := sn.Inventory(); strings.Join(got, ",") != "prod/db,prod/web" {
			t.Errorf("scan %v: inventory %v", scan, got)
		}
		rec, err := sn.Get("prod/web")
		if err != nil || rec == nil || rec.ID != "prod/web" || rec.Sum == "" {
			t.Errorf("scan %v: get %+v %v", scan, rec, err)
		}
		if rec, err := sn.Get("prod/new"); rec != nil || err != nil {
			t.Errorf("scan %v: get of a unit without a record: %+v %v", scan, rec, err)
		}
		if _, held, err := sn.Held("prod/web", Holder{Run: "r1"}); held || err != nil {
			t.Errorf("scan %v: held %v %v", scan, held, err)
		}
		if h, held, err := sn.Held("prod/db", Holder{Run: "r1"}); !held || h.Run != "r2" || err != nil {
			t.Errorf("scan %v: held %v by %+v %v", scan, held, h, err)
		}
		want := "list prod/,get prod/web/state.json,lock prod/db/state.lock"
		if scan {
			want = "scan prod/,lock prod/db/state.lock"
		}
		m.mu.Lock()
		if got := strings.Join(m.calls, ","); got != want {
			t.Errorf("scan %v: operations %s, not %s", scan, got, want)
		}
		m.mu.Unlock()
	}
}
