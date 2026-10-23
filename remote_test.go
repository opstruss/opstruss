package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
)

// sourceRepo installs only a test Git shim; production still receives and checks
// the canonical HTTPS URL and never accepts a fixture repository address.
func sourceRepo(ts *testscript.TestScript, _ bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: sourcerepo <directory>")
	}
	repo := ts.MkAbs(args[0])
	git, err := exec.LookPath("git")
	ts.Check(err)
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"add", "."}, {"commit", "-qm", "v1"}, {"tag", "v1"}} {
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			ts.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	shim := ts.MkAbs("gitshim")
	ts.Check(os.MkdirAll(shim, 0o755))
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
ls-remote)
  test "$3" = https://github.com/opstruss/plugins.git
  test ! -f '%s/offline'
  shift 3
  exec '%s' -c protocol.file.allow=always ls-remote --exit-code '%s' "$@"
  ;;
fetch)
  test "$4" = https://github.com/opstruss/plugins.git
  test ! -f '%s/offline'
  exec '%s' -c protocol.file.allow=always fetch --no-tags --depth=1 '%s' "$5"
  ;;
*) exec '%s' "$@" ;;
esac
`, ts.MkAbs("."), git, repo, ts.MkAbs("."), git, repo, git)
	ts.Check(os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755))
	ts.Setenv("PATH", shim+string(os.PathListSeparator)+ts.Getenv("PATH"))
}

type assetTransport struct {
	base http.RoundTripper
	dir  string
}

func (f assetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" && r.URL.Host == "github.com" && strings.HasPrefix(r.URL.Path, "/opstruss/plugins/releases/download/") {
		if _, err := os.Stat(filepath.Join(f.dir, "offline")); err == nil {
			return nil, fmt.Errorf("fixture offline")
		}
		file, err := os.Open(filepath.Join(f.dir, filepath.Base(r.URL.Path)))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: struct {
			io.Reader
			io.Closer
		}{file, file}, Request: r}, nil
	}
	return f.base.RoundTrip(r)
}

func fixtureAssets() {
	if dir := os.Getenv("OPSTRUSS_TEST_ASSETS"); dir != "" {
		http.DefaultTransport = assetTransport{http.DefaultTransport, dir}
	}
}

func writableFixtureCache(root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

func ageCache(ts *testscript.TestScript, _ bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: agecache <cache directory>")
	}
	metas, err := filepath.Glob(filepath.Join(ts.MkAbs(args[0]), "meta", "*.json"))
	ts.Check(err)
	for _, name := range metas {
		b, err := os.ReadFile(name)
		ts.Check(err)
		var m map[string]any
		ts.Check(json.Unmarshal(b, &m))
		m["used"] = time.Now().UTC().Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
		b, err = json.Marshal(m)
		ts.Check(err)
		ts.Check(os.WriteFile(name, b, 0o600))
	}
}
