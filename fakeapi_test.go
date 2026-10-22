package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeapi is an HTTP API that answers as the services of the use cases
// would, for tests:
//
//	fakeapi [-listen addr] [-tls] [-days n] <dir>
//
// It answers from <dir>/routes, read at each request: one route a line,
// METHOD PATH STATUS BODY, where a PATH ending in * matches every path it
// begins and a BODY of @<file> is that file of <dir>. The lines of the
// first METHOD and PATH that match answer in turn, the last one from then
// on, counted again whenever the file changes. Each request goes to <dir>/requests as one line: METHOD PATH?QUERY,
// auth=<Authorization> when it has one, and the body. With -tls it answers
// HTTPS for localhost and 127.0.0.1 with a certificate valid for -days
// days, signed by a CA that it writes to <dir>/ca.pem. Once it listens, it
// writes its port to <dir>/port.
func fakeapi(args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "fakeapi: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("fakeapi", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:0", "the address to listen on")
	useTLS := fs.Bool("tls", false, "answer HTTPS")
	days := fs.Int("days", 90, "the days the certificate is valid")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return fail("usage: fakeapi [-listen addr] [-tls] [-days n] <dir>")
	}
	a := &api{dir: fs.Arg(0), seen: map[string]int{}}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail("%v", err)
	}
	if *useTLS {
		cert, err := a.certificate(*days)
		if err != nil {
			return fail("%v", err)
		}
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := a.write("port", []byte(port)); err != nil {
		return fail("%v", err)
	}
	return fail("%v", http.Serve(l, a))
}

type api struct {
	dir    string
	mu     sync.Mutex
	routes string         // the routes file as seen counts it
	seen   map[string]int // the requests each route has answered
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	line := r.Method + " " + r.URL.RequestURI()
	if auth := r.Header.Get("Authorization"); auth != "" {
		line += " auth=" + auth
	}
	if b := strings.TrimSpace(string(body)); b != "" {
		line += " " + strings.ReplaceAll(b, "\n", `\n`)
	}
	if f, err := os.OpenFile(filepath.Join(a.dir, "requests"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
		_, _ = fmt.Fprintln(f, line)
		_ = f.Close()
	}
	status, answer := a.route(r.Method, r.URL.Path)
	if strings.HasPrefix(answer, "{") || strings.HasPrefix(answer, "[") {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

// route answers a request from the routes file: 404 when no route matches.
func (a *api) route(method, path string) (int, string) {
	data, _ := os.ReadFile(filepath.Join(a.dir, "routes"))
	if string(data) != a.routes {
		a.routes, a.seen = string(data), map[string]int{}
	}
	key, answers := "", []string{}
	for line := range strings.Lines(string(data)) {
		m, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		p, rest, _ := strings.Cut(strings.TrimSpace(rest), " ")
		prefix, wild := strings.CutSuffix(p, "*")
		if m != method || p != path && (!wild || !strings.HasPrefix(path, prefix)) {
			continue
		}
		if key == "" {
			key = m + " " + p
		}
		if key == m+" "+p {
			answers = append(answers, strings.TrimSpace(rest))
		}
	}
	if len(answers) == 0 {
		return http.StatusNotFound, `{"message": "no route"}`
	}
	answer := answers[min(a.seen[key], len(answers)-1)]
	a.seen[key]++
	code, body, _ := strings.Cut(answer, " ")
	status, err := strconv.Atoi(code)
	if err != nil {
		return http.StatusInternalServerError, "fakeapi: route " + key + " has no status"
	}
	if file, ok := strings.CutPrefix(body, "@"); ok {
		data, err := os.ReadFile(filepath.Join(a.dir, file))
		if err != nil {
			return http.StatusInternalServerError, err.Error()
		}
		body = string(data)
	}
	return status, body
}

// certificate makes a CA, writes it to ca.pem, and signs with it a
// certificate for localhost and 127.0.0.1 that is valid for days days.
func (a *api) certificate(days int) (tls.Certificate, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fakeapi CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 0, days),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := a.write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// write writes a file of the dir whole, so whoever waits for it never
// reads a part.
func (a *api) write(name string, data []byte) error {
	tmp := filepath.Join(a.dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(a.dir, name))
}
