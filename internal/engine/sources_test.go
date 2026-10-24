package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/opstruss/opstruss/internal/sources"
)

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCommandsReleaseSources(t *testing.T) {
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	for _, command := range []struct {
		name string
		run  func(context.Context, Options) (*Result, error)
	}{{"plan", PlanContext}, {"status", StatusUnits}, {"logs", Logs}, {"unlock", Unlock}} {
		t.Run(command.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Cleanup(func() {
				if err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
					if err == nil && d.IsDir() {
						return os.Chmod(p, 0o700)
					}
					return err
				}); err != nil {
					t.Error(err)
				}
			})
			payload := []byte("#!/bin/sh\nexit 0\n")
			sum := sha256.Sum256(payload)
			binary := sources.Binary{URL: "https://github.com/opstruss/catalog/releases/download/v1/tool", SHA256: hex.EncodeToString(sum[:])}
			manifest := fmt.Sprintf("name: demo\nbin:\n  demo-tool:\n    %s: {url: %s, sha256: %s}\ntasks:\n  up:\n    run: [echo, ok]\n", sources.Platform(), binary.URL, binary.SHA256)
			if err := os.Mkdir(filepath.Join(project, "demo"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "demo", "opstruss.plugin.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte("name: sources\nuses:\n  demo: ./demo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = sourceTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			ctx := t.Context()
			if _, err := command.run(ctx, Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv}); err != nil {
				t.Fatal(err)
			}
			cache, err := sources.UserCache()
			if err != nil {
				t.Fatal(err)
			}
			dir, lease, err := cache.Binary(ctx, "demo-tool", binary, false, false)
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
			if err := os.Chmod(filepath.Join(dir, "demo-tool"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, lease, err = cache.Binary(ctx, "demo-tool", binary, true, false)
			if err != nil {
				t.Fatalf("completed %s still prevents cache repair: %v", command.name, err)
			}
			lease.Close()
		})
	}
}
