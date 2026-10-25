package plugin

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/model"
)

// TestMain makes the test binary a program that leaves the process group
// of the program that started it while it keeps its output, as a careless
// daemon does.
func TestMain(m *testing.M) {
	if os.Getenv("PLUGIN_HELPER") == "escape" {
		_ = syscall.Setpgid(0, 0)
		_ = os.WriteFile(os.Getenv("PLUGIN_HELPER_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func newRun(t *testing.T, environ ...string) *Run {
	t.Helper()
	root := t.TempDir()
	r := &Run{Project: "app", Stack: "dev", ID: "20261020T101530Z-4b2e91d0", Parents: []string{"20261020T101500Z-00000001"},
		Temp: filepath.Join(root, "tmp", "20261020T101530Z-4b2e91d0"), Artifacts: filepath.Join(root, "artifacts"),
		Environ: append([]string{"PATH=" + os.Getenv("PATH")}, environ...)}
	if err := r.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func run(t *testing.T, r *Run, c *Call) *Result {
	t.Helper()
	if c.Base == "" {
		c.Base = t.TempDir()
	}
	res, err := r.Exec(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func script(s string) model.Command { return model.Command{Script: s} }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestEnvironment(t *testing.T) {
	r := newRun(t, "HOME=/home/op", "LC_ALL=C", "AWS_PROFILE=dev", "AWS_REGION=eu", "SECRET=x",
		"OPSTRUSS_RUN=parent", "INPUT_X=leak")
	work := filepath.Join(t.TempDir(), "work", "dev", "web")
	call := func(cmd model.Command, outputs map[string]*model.Output) *Result {
		return run(t, r, &Call{
			Command: cmd,
			Env:     map[string]model.EnvValue{"NODE_ENV": {Text: "production"}, "KUBECONFIG": {Text: "apiVersion: v1", File: true}},
			Pass:    []string{"AWS_*"},
			Task:    "up", Reason: "changed", Digest: "sha256:abc",
			Unit: &Unit{Address: "dev/web", StateKey: "dev/web.7f3a9c1e", Workdir: work, State: map[string]any{"outputs": map[string]any{}}},
			Inputs: map[string]any{"log-level": "debug", "replicas": int64(2), "tags": []any{"a"},
				"big": strings.Repeat("x", MaxInputEnv+1), "none": nil},
			Outputs: outputs,
		})
	}
	res := call(model.Command{Argv: []string{"env"}}, map[string]*model.Output{"env": {From: "stdout", Format: "dotenv"}})
	env, _ := res.Outputs["env"].(map[string]any)
	want := map[string]string{
		"HOME": "/home/op", "LC_ALL": "C", "AWS_PROFILE": "dev", "AWS_REGION": "eu", "NODE_ENV": "production",
		"OPSTRUSS_CONTRACT": "1", "OPSTRUSS_PROJECT": "app", "OPSTRUSS_STACK": "dev", "OPSTRUSS_RUN": r.ID,
		"OPSTRUSS_RUN_PARENTS": "20261020T101500Z-00000001", "OPSTRUSS_TASK": "up", "OPSTRUSS_REASON": "changed",
		"OPSTRUSS_DIGEST": "sha256:abc", "OPSTRUSS_ADDRESS": "dev/web", "OPSTRUSS_STATE_KEY": "dev/web.7f3a9c1e",
		"OPSTRUSS_TEMP": r.Temp, "OPSTRUSS_ARTIFACTS": r.Artifacts, "OPSTRUSS_WORKDIR": work,
		"INPUT_LOG_LEVEL": "debug", "INPUT_REPLICAS": "2", "INPUT_TAGS": `["a"]`,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %v, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"SECRET", "INPUT_X", "INPUT_BIG", "INPUT_NONE", "OPSTRUSS_PLAN"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is set", k)
		}
	}
	for _, k := range []string{"OPSTRUSS_OUTPUT", "OPSTRUSS_RESULT", "OPSTRUSS_INPUTS", "OPSTRUSS_STATE", "KUBECONFIG"} {
		p, _ := env[k].(string)
		if !strings.HasPrefix(p, r.Temp+"/") {
			t.Errorf("%s = %q, want a file in the run's directory", k, p)
		}
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is left after the call: %v", k, err)
		}
	}

	res = call(script(`{
  echo "inputs=$(cat "$OPSTRUSS_INPUTS")"
  echo "state=$(cat "$OPSTRUSS_STATE")"
  echo "kube=$(cat "$KUBECONFIG")"
  echo "mode=$(ls -l "$KUBECONFIG" | cut -c1-10)"
  test -e /dev/fd/3 && echo fd3=open
} >> "$OPSTRUSS_OUTPUT"`), map[string]*model.Output{
		"inputs": {Type: "object"}, "state": {Type: "object"}, "kube": {}, "mode": {}, "fd3": {},
	})
	if res.OutputErr != nil {
		t.Fatal(res.OutputErr)
	}
	inputs := res.Outputs["inputs"].(map[string]any)
	if inputs["replicas"] != int64(2) || len(inputs["big"].(string)) != MaxInputEnv+1 {
		t.Errorf("OPSTRUSS_INPUTS: %.80v", inputs)
	}
	for k, v := range map[string]any{"state": map[string]any{"outputs": map[string]any{}}, "kube": "apiVersion: v1", "mode": "-rw-------", "fd3": "open"} {
		if !reflect.DeepEqual(res.Outputs[k], v) {
			t.Errorf("%s = %v, want %v", k, res.Outputs[k], v)
		}
	}
}

// A program gets NO_COLOR unless the run shows its output in color, and
// whenever an output reads one of its streams; env: sets it as it says.
func TestNoColor(t *testing.T) {
	for _, c := range []struct {
		color, reads bool
		env          map[string]model.EnvValue
		want         string
	}{
		{false, false, nil, "1"},
		{true, false, nil, "unset"},
		{true, true, nil, "1"},
		{false, false, map[string]model.EnvValue{"NO_COLOR": {Text: ""}}, ""},
	} {
		r := newRun(t)
		r.Color = c.color
		outputs := map[string]*model.Output{"no_color": {}}
		if c.reads {
			outputs["out"] = &model.Output{From: "stdout"}
		}
		res := run(t, r, &Call{Command: script(`echo "no_color=${NO_COLOR-unset}" >> "$OPSTRUSS_OUTPUT"`), Env: c.env, Outputs: outputs})
		if got := res.Outputs["no_color"]; got != c.want {
			t.Errorf("color %v, reads %v, env %v: NO_COLOR %v, want %q", c.color, c.reads, c.env, got, c.want)
		}
	}
}

// Passed hears the variables that pass: lets through to the program, empty
// ones too, and not one that env: sets or one that no pattern names.
func TestPassed(t *testing.T) {
	r := Outside("app", "dev", []string{"PATH=" + os.Getenv("PATH"), "API_TOKEN=api", "DB_TOKEN=", "SET_TOKEN=operator", "OTHER=x", "OPSTRUSS_RUN=r"})
	got := map[string]string{}
	r.Passed = func(name, value string) { got[name] = value }
	if _, err := r.Prepare(&Call{Command: model.Command{Argv: []string{"true"}}, Base: t.TempDir(), Pass: []string{"*_TOKEN", "OPSTRUSS_*"},
		Env: map[string]model.EnvValue{"SET_TOKEN": {Text: "inline"}}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"API_TOKEN": "api", "DB_TOKEN": ""}; !maps.Equal(got, want) {
		t.Fatalf("passed %v, want %v", got, want)
	}
}

func TestOutputs(t *testing.T) {
	r := newRun(t)
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "image.json"), []byte(`{"tag": "1.2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	decl := map[string]*model.Output{
		"name": {}, "count": {Type: "integer"}, "ok": {Type: "boolean"}, "notes": {}, "tags": {Type: "array"},
		"report": {From: "stdout", Format: "json"}, "image": {From: "file", Path: "image.json", Format: "json"},
		"gone": {From: "file", Path: "missing.json"}, "unset": {Type: "string"},
	}
	res := run(t, r, &Call{Base: base, Outputs: decl, Command: script(`cat >> "$OPSTRUSS_OUTPUT" <<'OUT'
name=web
count=3
ok=true
tags=["a","b"]
notes<<EOF
line one
line two
EOF
extra=1
OUT
echo '{"changed": false, "message": "nothing to do", "later": 1}' > "$OPSTRUSS_RESULT"
echo '{"critical": 0}'`)})
	if res.OutputErr != nil {
		t.Fatal(res.OutputErr)
	}
	want := map[string]any{"name": "web", "count": int64(3), "ok": true, "notes": "line one\nline two", "tags": []any{"a", "b"},
		"report": map[string]any{"critical": int64(0)}, "image": map[string]any{"tag": "1.2"}, "gone": nil, "unset": nil}
	if !reflect.DeepEqual(res.Outputs, want) {
		t.Fatalf("outputs %#v", res.Outputs)
	}
	if res.Changed || res.Message != "nothing to do" {
		t.Fatalf("result %v %q", res.Changed, res.Message)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "extra") {
		t.Fatalf("warnings %q", res.Warnings)
	}

	// A program that fails keeps what it wrote; a wrong type is an error.
	res = run(t, r, &Call{Outputs: decl, Command: script(`echo '{"count": "three", "name": "web"}' > "$OPSTRUSS_OUTPUT"; exit 4`)})
	if res.ExitCode != 4 || res.OutputErr == nil || !strings.Contains(res.OutputErr.Error(), "output count") {
		t.Fatalf("exit %d, error %v", res.ExitCode, res.OutputErr)
	}
	if res.Outputs["name"] != "web" || string(res.RawOutput) != `{"count": "three", "name": "web"}`+"\n" || !res.Changed {
		t.Fatalf("outputs %v, raw %q", res.Outputs, res.RawOutput)
	}

	res = run(t, r, &Call{Outputs: map[string]*model.Output{"all": {From: "stdout"}},
		Command: script(`head -c 1048577 /dev/zero | tr '\0' x`)})
	if res.OutputErr == nil || !strings.Contains(res.OutputErr.Error(), "more than 1 MiB") || res.Outputs["all"] != nil {
		t.Fatalf("an output over 1 MiB: %v", res.OutputErr)
	}
}

func TestParseOutputFile(t *testing.T) {
	for in, want := range map[string]string{
		"a<<EOF\nb\n":       "no line EOF that ends it",
		"just text\n":       "line 1 is neither",
		`{"a": 1} {"b": 2}`: "not one JSON object",
	} {
		if _, _, err := ParseOutputFile([]byte(in), nil, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}

func TestStop(t *testing.T) {
	r := newRun(t)
	if res := run(t, r, &Call{Command: script(`kill -TERM $$`)}); res.ExitCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("a program that SIGTERM ended exits %d", res.ExitCode)
	}

	// A cancel sends stop.signal, and the program's trap decides how it ends.
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		eventually(t, "the trap", func() bool { _, err := os.Stat(ready); return err == nil })
		cancel()
	}()
	res, err := r.Exec(ctx, &Call{Base: t.TempDir(), Stop: Stop{Signal: syscall.SIGTERM, Grace: 5 * time.Second},
		Outputs: map[string]*model.Output{"stopped": {}},
		Command: script(`trap 'echo stopped=yes >> "$OPSTRUSS_OUTPUT"; exit 5' TERM; touch ` + ready + `; while :; do sleep 0.05; done`)})
	if err != nil || res.ExitCode != 5 || res.Outputs["stopped"] != "yes" {
		t.Fatalf("cancel: %v %+v", err, res)
	}

	// A program that ignores it is killed after the grace period.
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	res, err = r.Exec(ctx, &Call{Base: t.TempDir(), Stop: Stop{Signal: syscall.SIGINT, Grace: 100 * time.Millisecond},
		Command: script(`trap '' INT; sleep 30`)})
	if err != nil || res.ExitCode != 128+int(syscall.SIGKILL) || time.Since(began) > 5*time.Second {
		t.Fatalf("timeout: %v %+v after %s", err, res, time.Since(began))
	}

	// What a program leaves in its group stops with it.
	pidFile := filepath.Join(t.TempDir(), "pid")
	res = run(t, r, &Call{Stop: Stop{Signal: syscall.SIGTERM, Grace: time.Second}, Command: script(`sleep 30 & echo $! > ` + pidFile)})
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	eventually(t, "the background sleep to end", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "left in its process group") {
		t.Fatalf("warnings %q", res.Warnings)
	}
}

func TestLeftGroup(t *testing.T) {
	r := newRun(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	began := time.Now()
	res := run(t, r, &Call{
		Env:     map[string]model.EnvValue{"PLUGIN_HELPER": {Text: "escape"}, "PLUGIN_HELPER_PID": {Text: pidFile}, "HELPER": {Text: os.Args[0]}},
		Command: script(`"$HELPER" & while ! test -s "$PLUGIN_HELPER_PID"; do sleep 0.01; done`),
	})
	var pid int
	eventually(t, "the helper's pid", func() bool {
		data, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(string(data))
		return pid > 0
	})
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if d := time.Since(began); d < PipeWait || d > PipeWait+3*time.Second {
		t.Fatalf("the call took %s", d)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "still holds its output") }) {
		t.Fatalf("warnings %q", res.Warnings)
	}

	// The helper still has the run's lock, as a program of a run whose
	// OpsTruss was killed does.
	if err := r.tracker.Close(); err != nil {
		t.Fatal(err)
	}
	m := Marker(r.ID, r.Temp)
	if _, held, err := m.Alive(); err != nil || !held {
		t.Fatalf("while the helper runs: %v %v", held, err)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	eventually(t, "the lock to be free", func() bool { _, held, _ := m.Alive(); return !held })
}

func TestStdin(t *testing.T) {
	r := newRun(t)
	in, err := Encode("hello", "text")
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, r, &Call{Stdin: in, Command: model.Command{Argv: []string{"cat"}},
		Outputs: map[string]*model.Output{"echo": {From: "stdout"}}})
	if res.Outputs["echo"] != "hello" {
		t.Fatalf("stdin read back as %q", res.Outputs["echo"])
	}
	// A program that does not read its stdin does not block the call.
	res = run(t, r, &Call{Stdin: make([]byte, 1<<20), Command: model.Command{Argv: []string{"true"}}})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
}

func TestProgram(t *testing.T) {
	r := newRun(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	tool := "#!/bin/sh\necho dir=\"$(basename \"$PWD\")\" >> \"$OPSTRUSS_OUTPUT\"\n"
	if err := os.WriteFile(filepath.Join(base, "app", "tool"), []byte(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	// A relative program is read from the working directory, as a shell does.
	res := run(t, r, &Call{Base: base, Dir: "app", Command: model.Command{Argv: []string{"./tool"}},
		Outputs: map[string]*model.Output{"dir": {}}})
	if res.Outputs["dir"] != "app" {
		t.Fatalf("outputs %v, error %v", res.Outputs, res.OutputErr)
	}
	for _, c := range []*Call{
		{Base: base, Command: model.Command{Argv: []string{"no-such-program"}}},
		{Base: base, Dir: "missing", Command: model.Command{Argv: []string{"true"}}},
		{Base: base, Shell: "fish", Command: script("true")},
	} {
		if _, err := r.Exec(context.Background(), c); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
	if _, err := LookPath("sh", "relative/bin"); err == nil {
		t.Error("a program found through a relative PATH directory")
	}
}

func TestShells(t *testing.T) {
	for shell, want := range map[string][]string{
		"":       {"sh", "-e", "-c", "x"},
		"bash":   {"bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", "x"},
		"python": {"python3", "-c", "x"},
		"node":   {"node", "-e", "x"},
		"pwsh":   {"pwsh", "-NoProfile", "-NonInteractive", "-Command", "x"},
	} {
		if got, err := argv(script("x"), shell, nil); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q %v", shell, got, err)
		}
	}
	// The argv of opstruss exec follows the program's own arguments; a
	// script of sh gets it as "$@".
	if got, _ := argv(model.Command{Argv: []string{"./x", "a"}}, "", []string{"b"}); !slices.Equal(got, []string{"./x", "a", "b"}) {
		t.Errorf("argv: %q", got)
	}
	if got, _ := argv(script("x"), "", []string{"b"}); !slices.Equal(got, []string{"sh", "-e", "-c", "x", "opstruss", "b"}) {
		t.Errorf("script: %q", got)
	}
	r := newRun(t)
	// sh -e stops at the first command that fails.
	if res := run(t, r, &Call{Command: script("false\necho after=1 >> \"$OPSTRUSS_OUTPUT\""),
		Outputs: map[string]*model.Output{"after": {}}}); res.ExitCode != 1 || res.Outputs["after"] != nil {
		t.Fatalf("sh: exit %d, outputs %v", res.ExitCode, res.Outputs)
	}
	if _, err := LookPath("bash", os.Getenv("PATH")); err == nil {
		if res := run(t, r, &Call{Shell: "bash", Command: script("false | true")}); res.ExitCode != 1 {
			t.Fatalf("bash pipefail: exit %d", res.ExitCode)
		}
	}
}

func TestFormats(t *testing.T) {
	for _, tt := range []struct {
		format, in string
		want       any
	}{
		{"text", "a\n\n", "a"},
		{"lines", "a\nb\n", []any{"a", "b"}},
		{"lines", "", []any{}},
		{"json", " \n", nil},
		{"json", `{"n": 1.5}`, map[string]any{"n": 1.5}},
		{"yaml", "a: 1\nb: [x]\n", map[string]any{"a": int64(1), "b": []any{"x"}}},
		{"dotenv", "# c\nexport A=1\nB = 'x y'\nC=\"l1\\nl2\"\n", map[string]any{"A": "1", "B": "x y", "C": "l1\nl2"}},
		{"text", "bad \xff", "bad \uFFFD"},
	} {
		got, err := Decode([]byte(tt.in), tt.format)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("decode %s %q: %#v %v", tt.format, tt.in, got, err)
		}
	}
	env := map[string]any{"A": "plain", "B": "has space", "C": "it's", "D": "multi\nline", "E": int64(3)}
	out, err := Encode(env, "dotenv")
	if want := "A=plain\nB='has space'\nC=\"it's\"\nD=\"multi\\nline\"\nE=3\n"; err != nil || string(out) != want {
		t.Fatalf("dotenv: %q %v", out, err)
	}
	if back, _ := Decode(out, "dotenv"); back.(map[string]any)["D"] != "multi\nline" {
		t.Fatalf("dotenv read back: %v", back)
	}
	for _, tt := range []struct {
		v      any
		format string
		want   string
	}{
		{"a", "text", "a\n"},
		{map[string]any{"b": 1.5, "a": true}, "json", `{"a":true,"b":1.5}` + "\n"},
		{[]any{"a", int64(2)}, "lines", "a\n2\n"},
	} {
		if out, err := Encode(tt.v, tt.format); err != nil || string(out) != tt.want {
			t.Errorf("encode %v as %s: %q %v", tt.v, tt.format, out, err)
		}
	}
	if _, err := Encode([]any{"a\nb"}, "lines"); err == nil {
		t.Error("lines wrote an item with a newline")
	}
}

func TestStopOf(t *testing.T) {
	if s, err := StopOf(nil); err != nil || s != DefaultStop {
		t.Fatalf("default: %v %v", s, err)
	}
	s, err := StopOf(&model.Stop{Signal: "SIGTERM", Grace: "5s"})
	if err != nil || s != (Stop{Signal: syscall.SIGTERM, Grace: 5 * time.Second}) {
		t.Fatalf("%v %v", s, err)
	}
}
