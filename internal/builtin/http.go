package builtin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/plugin"
)

// httpCall sends one request and reads one response: the body is parsed
// when it is JSON, header names are lower case, and an https response
// gives its TLS version and certificates, leaf first. A status that
// status_code does not accept fails the call with the outputs kept.
func httpCall(ctx context.Context, in map[string]any) *Result {
	if t, ok := in["timeout"].(string); ok {
		d, err := time.ParseDuration(t)
		if err != nil {
			return failed(fmt.Errorf("timeout: %w", err))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	method := strings.ToUpper(in["method"].(string))
	var body io.Reader
	jsonBody := false
	switch b := in["body"].(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	default:
		body, jsonBody = strings.NewReader(expr.JSON(b)), true
	}
	req, err := http.NewRequestWithContext(ctx, method, in["url"].(string), body)
	if err != nil {
		return failed(err)
	}
	headers, _ := in["headers"].(map[string]any)
	for k, v := range headers {
		s, ok := v.(string)
		if !ok {
			return failed(fmt.Errorf("headers.%s is %s; header values are strings", k, expr.JSON(v)))
		}
		req.Header.Set(k, s)
	}
	if jsonBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client, err := clientFor(in["ca"])
	if err != nil {
		return failed(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return failed(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, plugin.MaxOutputs+1))
	if err != nil {
		return failed(err)
	}
	if len(data) > plugin.MaxOutputs {
		return failed(fmt.Errorf("the body of the response is larger than %d bytes", plugin.MaxOutputs))
	}
	out := map[string]any{"status": int64(resp.StatusCode), "headers": headerMap(resp.Header)}
	out["body"], err = decodeBody(resp.Header.Get("Content-Type"), data)
	if err != nil {
		return failed(fmt.Errorf("body: %w", err))
	}
	if resp.TLS != nil {
		out["tls"] = tlsOf(resp.TLS)
	}
	res := &Result{Outputs: out, Changed: method != http.MethodGet && method != http.MethodHead}
	ok, err := accepts(in["status_code"], resp.StatusCode)
	switch {
	case err != nil:
		res.Err = err
	case !ok:
		res.Err = fmt.Errorf("%s %s answered %d, which status_code does not accept", method, req.URL.Redacted(), resp.StatusCode)
	}
	return res
}

// clientFor is the default client, or one that also trusts the PEM
// certificates in ca besides the roots of the system.
func clientFor(ca any) (*http.Client, error) {
	pem, _ := ca.(string)
	if pem == "" {
		return http.DefaultClient, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("ca: %w", err)
	}
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, errors.New("ca holds no PEM certificate")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: t}, nil
}

func headerMap(h http.Header) map[string]any {
	out := map[string]any{}
	for k, v := range h {
		out[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return out
}

func decodeBody(contentType string, data []byte) (any, error) {
	media, _, _ := mime.ParseMediaType(contentType)
	if media == "application/json" || strings.HasSuffix(media, "+json") {
		return plugin.Decode(data, "json")
	}
	return string(bytes.ToValidUTF8(data, []byte("\uFFFD"))), nil
}

func tlsOf(st *tls.ConnectionState) map[string]any {
	var certs []any
	for _, c := range st.PeerCertificates {
		names := make([]any, len(c.DNSNames))
		for i, n := range c.DNSNames {
			names[i] = n
		}
		certs = append(certs, map[string]any{"subject": c.Subject.String(), "issuer": c.Issuer.String(),
			"not_before": c.NotBefore.UTC().Format(time.RFC3339), "not_after": c.NotAfter.UTC().Format(time.RFC3339),
			"dns_names": names})
	}
	return map[string]any{"version": tls.VersionName(st.Version), "certificates": certs}
}

// accepts says whether status_code, a status, a pattern such as 2xx, or a
// list of them, accepts status.
func accepts(codes any, status int) (bool, error) {
	list, ok := codes.([]any)
	if !ok {
		list = []any{codes}
	}
	for _, c := range list {
		switch v := c.(type) {
		case int64:
			if int(v) == status {
				return true, nil
			}
		case string:
			if len(v) == 3 && strings.HasSuffix(v, "xx") && v[0] >= '1' && v[0] <= '5' {
				if status/100 == int(v[0]-'0') {
					return true, nil
				}
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return false, fmt.Errorf("status_code takes statuses and patterns such as 2xx, not %q", v)
			}
			if n == status {
				return true, nil
			}
		default:
			return false, fmt.Errorf("status_code takes statuses and patterns such as 2xx, not %s", expr.JSON(c))
		}
	}
	return false, nil
}
