package sources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func archive(t *testing.T, headers []*tar.Header, contents [][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if i < len(contents) && len(contents[i]) > 0 {
			if _, err := tw.Write(contents[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBinaryCacheAndArchive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		if err := removeEntry(home); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	cache, _ := UserCache()
	payload := archive(t, []*tar.Header{{Name: "demo", Mode: 0o755, Size: 4}, {Name: "LICENSE", Mode: 0o644, Size: 2}}, [][]byte{[]byte("tool"), []byte("ok")})
	b := Binary{URL: "https://github.com/opstruss/catalog/releases/download/v1/demo.tar.gz", SHA256: payloadHash(payload)}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload)), Header: http.Header{}}, nil
	})
	dir, l, err := cache.Binary(ctx, "demo", b, false, false)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	exe, err := os.ReadFile(filepath.Join(dir, "demo"))
	if err != nil || string(exe) != "tool" {
		t.Fatalf("executable: %q %v", exe, err)
	}
	_, l, err = cache.Binary(ctx, "demo", b, false, false)
	if err != nil || calls != 1 {
		t.Fatalf("warm reuse: %d %v", calls, err)
	}
	l.Close()
	if err := os.Chmod(filepath.Join(dir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo"), []byte("evil"), 0o555); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Binary(ctx, "demo", b, false, false); err == nil {
		t.Fatal("changed executable accepted")
	}
	_, l, err = cache.Binary(ctx, "demo", b, true, false)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	for _, h := range []*tar.Header{{Name: "../demo", Mode: 0o755}, {Name: "/demo", Mode: 0o755}, {Name: "demo", Typeflag: tar.TypeSymlink, Linkname: "other"}, {Name: "demo", Typeflag: tar.TypeLink, Linkname: "other"}, {Name: "demo", Typeflag: tar.TypeChar}, {Name: "nested/demo", Mode: 0o755}} {
		p := archive(t, []*tar.Header{h}, nil)
		candidate := b
		candidate.SHA256 = payloadHash(p)
		if _, err := executable("demo", candidate, p); err == nil {
			t.Errorf("accepted %v", h)
		}
	}
	p := archive(t, []*tar.Header{{Name: "demo", Mode: 0o755}, {Name: "demo", Mode: 0o755}}, nil)
	candidate := b
	candidate.SHA256 = payloadHash(p)
	if _, err := executable("demo", candidate, p); err == nil {
		t.Fatal("accepted duplicate")
	}
}

func TestConcurrentBinaryPublicationSharesActiveEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		if err := removeEntry(home); err != nil {
			t.Error(err)
		}
	})
	cache, _ := UserCache()
	payload := []byte("executable")
	b := Binary{URL: "https://github.com/opstruss/catalog/releases/download/v1/demo", SHA256: payloadHash(payload)}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	downloading, finish := make(chan struct{}), make(chan struct{})
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		close(downloading)
		<-finish
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload))}, nil
	})
	type result struct {
		lease *Lease
		err   error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, l, err := cache.Binary(ctx, "demo", b, false, false); first <- result{l, err} }()
	<-downloading
	go func() { _, l, err := cache.Binary(ctx, "demo", b, false, false); second <- result{l, err} }()
	close(finish)
	a := <-first
	if a.err != nil {
		t.Fatal(a.err)
	}
	defer a.lease.Close()
	select {
	case b := <-second:
		if b.err != nil {
			t.Fatal(b.err)
		}
		b.lease.Close()
	case <-ctx.Done():
		t.Fatal("second consumer waited for the first consumer to release")
	}
}

func TestReleaseTokenDoesNotFollowRedirect(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Error("official request did not authenticate")
			}
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://release-assets.githubusercontent.com/asset"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked to redirect")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("payload")), Request: r}, nil
	})
	b := Binary{URL: "https://github.com/opstruss/catalog/releases/download/v1/demo", SHA256: payloadHash([]byte("payload"))}
	if _, err := download(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("requests: %d", calls)
	}
}
