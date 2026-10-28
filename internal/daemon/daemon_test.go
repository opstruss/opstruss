package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/opstruss/opstruss/internal/state"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func project(t *testing.T, dir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte("name: shop\nunits:\n  web:\n    run: echo up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestDaemon(t *testing.T) {
	for name, dir := range map[string]func() string{
		"socket in the project": func() string {
			d, err := os.MkdirTemp("/tmp", "d-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(d) })
			return d
		},
		"socket under TMPDIR": func() string { return filepath.Join(t.TempDir(), strings.Repeat("long", 20)) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := dir()
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			project(t, dir)
			root := filepath.Join(dir, ".opstruss")
			if c, err := Dial(root, "default", "v1", false); c != nil || err != nil {
				t.Fatalf("dial without a daemon: %v %v", c, err)
			}
			d, err := Start(Options{Dir: dir, Stack: "default", Version: "v1"})
			if err != nil {
				t.Fatal(err)
			}
			var running *RunningError
			if _, err := Start(Options{Dir: dir, Stack: "default", Version: "v1"}); !errors.As(err, &running) || running.PID != os.Getpid() {
				t.Fatalf("second daemon: %v", err)
			}
			f, err := Read(root, "default")
			if err != nil || f.PID != os.Getpid() || f.OpsTruss != "v1" {
				t.Fatalf("file %+v %v", f, err)
			}
			data, _ := os.ReadFile(filePath(root, "default"))
			doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if probs, err := schema.Validate(schema.Daemon, doc); err != nil || len(probs) > 0 {
				t.Fatalf("schema: %v %v", probs, err)
			}
			ctx, stop := context.WithCancel(context.Background())
			served := make(chan error)
			go func() { served <- d.Serve(ctx) }()
			c, err := Dial(root, "default", "v1", false)
			if err != nil || c == nil {
				t.Fatalf("dial: %v", err)
			}
			res, err := c.Call(context.Background(), "up", api.Request{Dir: dir, Environ: os.Environ(), Yes: true}, api.Handlers{})
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("up: %+v %v", res, err)
			}
			rec, err := record.Read(filepath.Join(root, "runs", "default", res.Run))
			if err != nil || rec.PID != os.Getpid() {
				t.Fatalf("record %+v %v", rec, err)
			}
			stop()
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			if Alive(root, "default") {
				t.Fatal("the lock is held after Serve")
			}
			if _, err := os.Stat(f.Socket); err == nil {
				t.Fatal("the socket is left")
			}
			log, _ := os.ReadFile(LogPath(root, "default"))
			if !bytes.Contains(log, []byte("daemon started")) || !bytes.Contains(log, []byte("daemon stopped")) {
				t.Fatalf("log %s", log)
			}
		})
	}
}

func TestDaemonServesItsStack(t *testing.T) {
	dir := project(t, t.TempDir())
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "opstruss.yaml"), []byte("name: other\nunits:\n  x:\n    run: echo x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, ".opstruss")
	d, err := Start(Options{Dir: dir, Stack: "default", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error)
	go func() { served <- d.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-served; err != nil {
			t.Error(err)
		}
	}()
	call := func(r api.Request) error {
		c, err := Dial(root, "default", "v1", false)
		if err != nil || c == nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = c.Close() }()
		r.Environ = os.Environ()
		_, err = c.Call(context.Background(), "up", r, api.Handlers{})
		return err
	}
	for _, c := range []struct {
		r    api.Request
		want string
	}{
		{api.Request{Dir: other, Yes: true}, "this daemon runs the project in " + dir},
		{api.Request{Dir: dir, Stack: "prod", Yes: true}, "the project has no stack prod"},
	} {
		if err := call(c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: %v", c.r, err)
		}
	}
	if _, err := os.Stat(filepath.Join(other, ".opstruss")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the other project was touched: %v", err)
	}
	if err := call(api.Request{Dir: alias, Yes: true}); err != nil {
		t.Fatalf("the project by another path: %v", err)
	}
}

func TestDaemonLosesLock(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "d-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	project(t, dir)
	d, err := Start(Options{Dir: dir, Stack: "default", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error)
	go func() { served <- d.Serve(context.Background()) }()
	if err := os.RemoveAll(filepath.Join(dir, ".opstruss")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon kept serving without its lock file")
	}
}

func TestReadNewer(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "daemon"), 0o700)
	_ = os.WriteFile(filePath(root, "prod"), []byte(`{"version": 9, "pid": "one"}`), 0o600)
	var newer *state.NewerError
	if _, err := Read(root, "prod"); !errors.As(err, &newer) {
		t.Fatalf("got %v", err)
	}
}
