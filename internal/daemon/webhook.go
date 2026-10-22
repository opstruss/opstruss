package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
)

// MaxBody is the largest body of a webhook request.
const MaxBody = 1 << 20

// skew is how far the timestamp of a signed request may be from now, so a
// request seen once cannot be played again later.
const skew = 5 * time.Minute

// webhooks is the HTTP server of the webhook triggers, on runtime.listen
// while the triggers installed have webhooks.
type webhooks struct {
	t *triggers

	mu     sync.Mutex
	addr   string // as runtime.listen writes it
	srv    *http.Server
	routes map[string]*engine.Trigger // by method and path
}

// set serves the webhooks of list on addr, in place of what it served.
func (w *webhooks) set(addr string, list []*engine.Trigger) error {
	routes := map[string]*engine.Trigger{}
	for _, tr := range list {
		if tr.Kind == "webhook" {
			routes[tr.Method+" "+tr.Path] = tr
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.routes = routes
	if len(routes) == 0 || addr != w.addr {
		w.close()
	}
	if len(routes) == 0 || w.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("webhooks: %w", err)
	}
	w.addr, w.srv = addr, &http.Server{Handler: w, ReadHeaderTimeout: 10 * time.Second}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(w.srv)
	actual := ln.Addr().String()
	_ = w.t.d.reg.update(func(f *File) { f.Listen = actual })
	w.t.d.event("trigger", "info", "", "webhooks listen on %s", actual)
	return nil
}

// close stops the server; w.mu is held.
func (w *webhooks) close() {
	if w.srv != nil {
		_ = w.srv.Close()
		w.srv, w.addr = nil, ""
		_ = w.t.d.reg.update(func(f *File) { f.Listen = "" })
	}
}

func (w *webhooks) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	tr := w.routes[r.Method+" "+r.URL.Path]
	known := false
	for _, other := range w.routes {
		known = known || other.Path == r.URL.Path
	}
	w.mu.Unlock()
	if tr == nil {
		code := map[bool]int{true: http.StatusMethodNotAllowed, false: http.StatusNotFound}[known]
		http.Error(rw, http.StatusText(code), code)
		return
	}
	refuse := func(code int, format string, args ...any) {
		w.t.d.event("trigger", "warn", tr.Task, "%s: refused a request from %s with %d: %s", tr, r.RemoteAddr, code, fmt.Sprintf(format, args...))
		http.Error(rw, http.StatusText(code), code)
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, MaxBody))
	if err != nil {
		refuse(http.StatusRequestEntityTooLarge, "the body is larger than %d bytes", MaxBody)
		return
	}
	if err := verify(tr.Signature, tr.Secret, r.Header, body, time.Now()); err != nil {
		refuse(http.StatusUnauthorized, "%v", err)
		return
	}
	if !w.t.begin(tr.Task) {
		rw.Header().Set("Retry-After", "1")
		refuse(http.StatusServiceUnavailable, "task %s has %d runs unfinished", tr.Task, MaxPending)
		return
	}
	ev := &engine.TriggerEvent{Trigger: "webhook", Time: time.Now().UTC(), Fields: fields(r, body)}
	started := make(chan string, 1)
	type done struct {
		res *engine.Result
		err error
	}
	finished := make(chan done, 1)
	w.t.runs.Add(1)
	go func() {
		defer w.t.runs.Done()
		defer w.t.end(tr.Task)
		opts := w.t.options()
		opts.Event, opts.Supervisor = ev, w.t.d.sup
		opts.Started = func(run string) { started <- run }
		res, err := engine.Fire(w.t.d.runs, opts, tr)
		w.t.report(tr, ev.Time, res, err)
		if res != nil {
			w.t.ended(res.Run, 0)
		}
		finished <- done{res, err}
	}()
	answer := func(code int, v any) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		_ = json.NewEncoder(rw).Encode(v)
	}
	if tr.Respond == "accepted" {
		select {
		case run := <-started:
			answer(http.StatusAccepted, map[string]any{"run": run})
			return
		case d := <-finished:
			w.answerDone(answer, d.res, d.err)
			return
		}
	}
	d := <-finished
	w.answerDone(answer, d.res, d.err)
}

// answerDone answers with how a run ended, or why there was none.
func (w *webhooks) answerDone(answer func(int, any), res *engine.Result, err error) {
	var withErr *engine.WithError
	switch {
	case errors.As(err, &withErr):
		answer(http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
	case err != nil:
		answer(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	case res == nil:
		answer(http.StatusAccepted, map[string]any{})
	default:
		code := http.StatusOK
		if res.ExitCode != 0 {
			code = http.StatusInternalServerError
		}
		answer(code, map[string]any{"run": res.Run, "outcome": res.Outcome, "exit_code": res.ExitCode, "outputs": res.Outputs})
	}
}

// fields are what event holds of a request: method, path, headers by
// lower-case name with values joined by ", ", query, and the body, parsed
// when its Content-Type is JSON.
func fields(r *http.Request, body []byte) map[string]any {
	headers := map[string]any{}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	query := map[string]any{}
	for k, v := range r.URL.Query() {
		query[k] = strings.Join(v, ", ")
	}
	var parsed any = string(body)
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && (mt == "application/json" || strings.HasSuffix(mt, "+json")) {
		var v any
		if json.Unmarshal(body, &v) == nil {
			parsed = v
		}
	}
	return map[string]any{"method": r.Method, "path": r.URL.Path, "headers": headers, "query": query, "body": parsed}
}

// verify checks a request against the secret of its webhook by the
// scheme of signature, before any run is made.
func verify(sig model.Signature, secret string, h http.Header, body []byte, now time.Time) error {
	key := []byte(secret)
	mac := func(alg func() hash.Hash, key, data []byte) []byte {
		m := hmac.New(alg, key)
		m.Write(data)
		return m.Sum(nil)
	}
	equal := func(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
	fresh := func(ts string) error {
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return errors.New("no timestamp")
		}
		if d := now.Sub(time.Unix(sec, 0)); d > skew || d < -skew {
			return fmt.Errorf("the timestamp is more than %s away", skew)
		}
		return nil
	}
	hexOf := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil
		}
		return b
	}
	switch sig.Scheme {
	case "github":
		got, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		if ok && equal(hexOf(got), mac(sha256.New, key, body)) {
			return nil
		}
		return errors.New("X-Hub-Signature-256 does not match")
	case "gitlab":
		if equal([]byte(h.Get("X-Gitlab-Token")), key) {
			return nil
		}
		return errors.New("X-Gitlab-Token does not match")
	case "bearer":
		got, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
		if ok && equal([]byte(got), key) {
			return nil
		}
		return errors.New("the bearer token does not match")
	case "slack":
		ts := h.Get("X-Slack-Request-Timestamp")
		if err := fresh(ts); err != nil {
			return err
		}
		got, ok := strings.CutPrefix(h.Get("X-Slack-Signature"), "v0=")
		if ok && equal(hexOf(got), mac(sha256.New, key, []byte("v0:"+ts+":"+string(body)))) {
			return nil
		}
		return errors.New("X-Slack-Signature does not match")
	case "stripe":
		var ts string
		var sigs []string
		for _, part := range strings.Split(h.Get("Stripe-Signature"), ",") {
			k, v, _ := strings.Cut(part, "=")
			switch k {
			case "t":
				ts = v
			case "v1":
				sigs = append(sigs, v)
			}
		}
		if err := fresh(ts); err != nil {
			return err
		}
		want := mac(sha256.New, key, []byte(ts+"."+string(body)))
		for _, s := range sigs {
			if equal(hexOf(s), want) {
				return nil
			}
		}
		return errors.New("Stripe-Signature does not match")
	case "standard":
		id, ts := h.Get("Webhook-Id"), h.Get("Webhook-Timestamp")
		if err := fresh(ts); err != nil {
			return err
		}
		if raw, ok := strings.CutPrefix(secret, "whsec_"); ok {
			if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
				key = b
			}
		}
		want := base64.StdEncoding.EncodeToString(mac(sha256.New, key, []byte(id+"."+ts+"."+string(body))))
		for _, s := range strings.Fields(h.Get("Webhook-Signature")) {
			if v, ok := strings.CutPrefix(s, "v1,"); ok && equal([]byte(v), []byte(want)) {
				return nil
			}
		}
		return errors.New("webhook-signature does not match")
	}
	alg := sha256.New
	if sig.Algorithm == "sha512" {
		alg = sha512.New
	}
	got, ok := strings.CutPrefix(h.Get(sig.Header), sig.Prefix)
	if !ok {
		return fmt.Errorf("%s does not match", sig.Header)
	}
	want := mac(alg, key, body)
	if sig.Encoding == "base64" {
		if b, err := base64.StdEncoding.DecodeString(got); err == nil && equal(b, want) {
			return nil
		}
	} else if equal(hexOf(got), want) {
		return nil
	}
	return fmt.Errorf("%s does not match", sig.Header)
}
