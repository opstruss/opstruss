package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/model"
)

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
