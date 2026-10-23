package daemon

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
)

// A body that does not come within readTimeout is answered 408, and one
// larger than MaxBody 413, before the signature is checked.
func TestWebhookBody(t *testing.T) {
	log, err := record.OpenLog(filepath.Join(t.TempDir(), "daemon.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	w := &webhooks{t: &triggers{d: &Daemon{log: log}},
		routes: map[string]*bound{"POST /hook": {tr: &engine.Trigger{Task: "t", Kind: "webhook", Method: "POST", Path: "/hook"}}}}
	srv := httptest.NewServer(w)
	defer srv.Close()
	old := readTimeout
	readTimeout = 200 * time.Millisecond
	defer func() { readTimeout = old }()

	for _, c := range []struct {
		head, body string
		code       int
	}{
		{"Content-Length: 100", "only some", http.StatusRequestTimeout},
		{"Content-Length: " + strconv.Itoa(MaxBody+1), strings.Repeat("x", MaxBody+1), http.StatusRequestEntityTooLarge},
	} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		go func() { _, _ = conn.Write([]byte("POST /hook HTTP/1.1\r\nHost: x\r\n" + c.head + "\r\n\r\n" + c.body)) }()
		start := time.Now()
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err != nil || res.StatusCode != c.code {
			t.Fatalf("%s: %v %v", c.head, res, err)
		}
		if c.code == http.StatusRequestTimeout && time.Since(start) > 2*time.Second {
			t.Fatalf("answered after %s", time.Since(start))
		}
	}
}

func sign(alg func() hash.Hash, key []byte, data string) []byte {
	m := hmac.New(alg, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func TestVerify(t *testing.T) {
	now := time.Unix(1792600000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	old := strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
	body := []byte(`{"a":1}`)
	whKey := []byte("standard-key")
	for _, tc := range []struct {
		name    string
		sig     model.Signature
		secret  string
		headers map[string]string
		ok      bool
	}{
		{"gitlab", model.Signature{Scheme: "gitlab"}, "s", map[string]string{"X-Gitlab-Token": "s"}, true},
		{"gitlab wrong", model.Signature{Scheme: "gitlab"}, "s", map[string]string{"X-Gitlab-Token": "x"}, false},
		{"slack", model.Signature{Scheme: "slack"}, "s", map[string]string{"X-Slack-Request-Timestamp": ts,
			"X-Slack-Signature": "v0=" + hex.EncodeToString(sign(sha256.New, []byte("s"), "v0:"+ts+":"+string(body)))}, true},
		{"slack replayed", model.Signature{Scheme: "slack"}, "s", map[string]string{"X-Slack-Request-Timestamp": old,
			"X-Slack-Signature": "v0=" + hex.EncodeToString(sign(sha256.New, []byte("s"), "v0:"+old+":"+string(body)))}, false},
		{"stripe", model.Signature{Scheme: "stripe"}, "s", map[string]string{"Stripe-Signature": "t=" + ts + ",v1=00,v1=" +
			hex.EncodeToString(sign(sha256.New, []byte("s"), ts+"."+string(body)))}, true},
		{"standard", model.Signature{Scheme: "standard"}, "whsec_" + base64.StdEncoding.EncodeToString(whKey), map[string]string{
			"Webhook-Id": "msg1", "Webhook-Timestamp": ts,
			"Webhook-Signature": "v1,AAAA v1," + base64.StdEncoding.EncodeToString(sign(sha256.New, whKey, "msg1."+ts+"."+string(body)))}, true},
		{"custom sha512 base64", model.Signature{Header: "X-Sig", Algorithm: "sha512", Encoding: "base64", Prefix: "sha512="}, "s",
			map[string]string{"X-Sig": "sha512=" + base64.StdEncoding.EncodeToString(sign(sha512.New, []byte("s"), string(body)))}, true},
		{"custom without prefix", model.Signature{Header: "X-Sig", Prefix: "sha256="}, "s",
			map[string]string{"X-Sig": hex.EncodeToString(sign(sha256.New, []byte("s"), string(body)))}, false},
	} {
		h := http.Header{}
		for k, v := range tc.headers {
			h.Set(k, v)
		}
		if err := verify(tc.sig, tc.secret, h, body, now); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
