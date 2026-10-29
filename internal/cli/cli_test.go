package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/spf13/cobra"
)

// ymlDir is a directory with opstruss.yml, which OpsTruss does not read.
func ymlDir(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yml"), []byte("name: app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRun(t *testing.T) {
	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, stdout: "Usage:"},
		{args: []string{"--help"}, stdout: "Usage:"},
		{args: []string{"help"}, stdout: "Usage:"},
		{args: []string{"version"}, stdout: "opstruss (devel) " + runtime.GOOS + "/" + runtime.GOARCH + "\n"},
		{args: []string{"version", "--json"}, stdout: `"os": "` + runtime.GOOS + `"`},
		{args: []string{"version", "extra"}, code: 1, stderr: `unknown command "extra"`},
		{args: []string{"nope"}, code: 1, stderr: `unknown command "nope"`},
		{args: []string{"pln"}, code: 1, stderr: `unknown command "pln" for "opstruss"; did you mean plan or run?`},
		{args: []string{"run", "nope"}, code: 1, stderr: `unknown command "nope" for "opstruss run"`},
		{args: []string{"run"}, stdout: "Usage:"},
		{args: []string{"--help"}, stdout: "Main commands:\n  init  "},
		{args: []string{"--version"}, stdout: "opstruss (devel) " + runtime.GOOS + "/" + runtime.GOARCH + "\n"},
		{args: []string{"up", "--help"}, stdout: "Examples:\n"},
		{args: []string{"up", "--help"}, stdout: "\n\nDocumentation: https://opstruss.github.io/dev/reference/cli/up\n"},
		{args: []string{"state", "rm", "--help"}, stdout: "Documentation: https://opstruss.github.io/dev/reference/cli/state-rm\n"},
		{args: []string{"--help"}, stdout: "a command.\n\nDocumentation: https://opstruss.github.io/dev/\n"},
		{args: []string{"--completion", "zsh"}, stdout: "#compdef opstruss"},
		{args: []string{"--completion", "csh"}, code: 1, stderr: "--completion takes bash, zsh, or fish"},
		{args: []string{"-C"}, code: 1, stderr: "-C needs a directory"},
		{args: []string{"version", "-d"}, code: 1, stderr: "-d is for up and tasks"},
		{args: []string{"-C", t.TempDir(), "validate"}, code: 1, stderr: "no opstruss.yaml in"},
		{args: []string{"-C", t.TempDir(), "plan", "--json"}, code: 1, stdout: `"type":"diagnostic","level":"error","message":"no opstruss.yaml in `},
		{args: []string{"-C", ymlDir(t), "validate"}, code: 1, stderr: "has opstruss.yml, which OpsTruss does not read: rename it to opstruss.yaml"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tt.args, &stdout, &stderr, nil); code != tt.code {
				t.Errorf("exit code = %d, want %d; stderr %q", code, tt.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tt.stdout)
			}
			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.stderr)
			}
		})
	}
}

// TestIsTerminal: /dev/null is a character device, as CI and service
// managers give it, and no terminal.
func TestIsTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Fatal("isTerminal(/dev/null) = true")
	}
}

// TestApprove: the question names what yes does.
func TestApprove(t *testing.T) {
	tests := []struct {
		q    engine.Question
		want string
	}{
		{engine.Question{Op: "up", Address: "default/web", Summary: "will run: new"}, "default/web: will run: new\nApply default/web? [y/N] "},
		{engine.Question{Op: "destroy", Address: "default/site", Summary: "will be destroyed"}, "default/site: will be destroyed\nDestroy default/site? [y/N] "},
		{engine.Question{Op: "destroy", Address: "default/job", Summary: "will be forgotten: it has no destroy program"},
			"default/job: will be forgotten: it has no destroy program\nForget default/job? [y/N] "},
		{engine.Question{Op: "task", Task: "deploy", Summary: "run task deploy"}, "deploy: run task deploy\nRun deploy? [y/N] "},
		{engine.Question{Address: "default/web", Summary: "will run: new"}, "default/web: will run: new\nApply default/web? [y/N] "},
		{engine.Question{Op: "up", Items: []engine.Question{
			{Op: "up", Address: "default/infra", Summary: "will run: new"},
			{Op: "up", Address: "default/config", Summary: "will run: new; plan after default/infra (env.DB)"},
			{Op: "destroy", Address: "default/old", Summary: "will be destroyed"},
			{Op: "destroy", Address: "default/job", Summary: "will be forgotten: it has no destroy program"},
		}}, "default/infra: will run: new\ndefault/config: will run: new; plan after default/infra (env.DB)\n" +
			"default/old: will be destroyed\ndefault/job: will be forgotten: it has no destroy program\n" +
			"Apply default/infra, default/config, destroy default/old, forget default/job? [y/N] "},
		{engine.Question{Op: "destroy", Items: []engine.Question{
			{Op: "destroy", Address: "default/shard[a]", Summary: "will be forgotten: it has no destroy program"},
			{Op: "destroy", Address: "default/shard[b]", Summary: "will be forgotten: it has no destroy program"},
			{Op: "destroy", Address: "default/sum", Summary: "will be forgotten: it has no destroy program"},
		}}, "default/shard[a]: will be forgotten: it has no destroy program\ndefault/shard[b]: will be forgotten: it has no destroy program\n" +
			"default/sum: will be forgotten: it has no destroy program\nForget 2 instances of default/shard, default/sum? [y/N] "},
	}
	for _, tt := range tests {
		var stderr strings.Builder
		a := &app{stderr: &stderr, stdin: bufio.NewReader(strings.NewReader("yes\n"))}
		ok, err := a.approve(context.Background(), tt.q)
		if err != nil || !ok {
			t.Fatalf("approve(%v) = %v, %v", tt.q, ok, err)
		}
		if stderr.String() != tt.want {
			t.Errorf("approve(%v) wrote %q, want %q", tt.q, stderr.String(), tt.want)
		}
	}
}

// TestApproveInstances: at a terminal, the question names the instances
// of one unit by their count, in one line.
func TestApproveInstances(t *testing.T) {
	var stdout, stderr strings.Builder
	a := &app{stdout: &stdout, stderr: &stderr, stdin: bufio.NewReader(strings.NewReader("n\n"))}
	a.renderers = map[io.Writer]*renderer{&stdout: newRenderer(&stdout, &stderr, false, true, false, false, false)}
	var items []engine.Question
	for i := range 300 {
		items = append(items, engine.Question{Op: "destroy", Address: fmt.Sprintf("default/shard[s%d]", i), Summary: "will be forgotten: it has no destroy program"})
	}
	items = append(items, engine.Question{Op: "destroy", Address: "default/sum", Summary: "will be forgotten: it has no destroy program"})
	if ok, err := a.approve(context.Background(), engine.Question{Op: "destroy", Items: items}); err != nil || ok {
		t.Fatalf("approve = %v, %v", ok, err)
	}
	if want := "Forget 300 instances of shard, sum? [y/N] "; stderr.String() != want {
		t.Errorf("asked %q, want %q", stderr.String(), want)
	}
}

// TestQuestionHoldsOutput: while a question waits, the output of the run
// waits for the answer, so the question stays the last line.
func TestQuestionHoldsOutput(t *testing.T) {
	r, w := io.Pipe()
	var stdout, stderr strings.Builder
	a := &app{stdout: &stdout, stderr: &stderr, stdin: bufio.NewReader(r)}
	done := make(chan bool)
	go func() {
		ok, _ := a.approve(context.Background(), engine.Question{Op: "up", Address: "default/a", Summary: "will run: new"})
		done <- ok
	}()
	for asking := false; !asking; {
		time.Sleep(time.Millisecond)
		a.renderMu.Lock()
		asking = a.asking
		a.renderMu.Unlock()
	}
	a.render()(engine.Event{Type: "log", Level: "info", Address: "default/b", Run: "r1", Message: "planning b"})
	a.render()(engine.Event{Type: "unit", Level: "warn", Address: "default/c", Run: "r1", Message: "slow"})
	a.renderMu.Lock()
	early := stdout.String()
	a.renderMu.Unlock()
	if early != "" {
		t.Fatalf("stdout while the question waits: %q", early)
	}
	if _, err := w.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("y did not approve")
	}
	if stdout.String() != "default/b | planning b\n" {
		t.Errorf("stdout %q", stdout.String())
	}
	if want := "default/a: will run: new\nApply default/a? [y/N] default/c: slow\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

// TestRepeatsPlan: the end of a turn that the plan line said already shows
// once, but again after another line of that unit, such as waiting for a
// lock.
func TestRepeatsPlan(t *testing.T) {
	var stdout strings.Builder
	a := &app{stdout: &stdout, stderr: &stdout}
	render := a.render()
	for _, ev := range []engine.Event{
		{Type: "plan", Level: "info", Address: "default/a", Run: "r", Message: "unchanged"},
		{Type: "plan", Level: "info", Address: "default/b", Run: "r", Message: "unchanged"},
		{Type: "unit", Level: "info", Address: "default/a", Run: "r", Message: "unchanged"},
		{Type: "unit", Level: "info", Address: "default/b", Run: "r", Message: "waiting for the lock held by run x"},
		{Type: "unit", Level: "info", Address: "default/b", Run: "r", Message: "unchanged"},
	} {
		render(ev)
	}
	want := "default/a: unchanged\ndefault/b: unchanged\ndefault/b: waiting for the lock held by run x\ndefault/b: unchanged\n"
	if stdout.String() != want {
		t.Errorf("got %q, want %q", stdout.String(), want)
	}
}

func TestTakeShort(t *testing.T) {
	a := &app{}
	args, err := a.takeShort([]string{"-C", "/srv", "-Cops", "up", "-d", "web", "--", "-C", "x", "-d"})
	if err != nil {
		t.Fatal(err)
	}
	if a.dir != "/srv/ops" || !a.detach || strings.Join(args, " ") != "up web -- -C x -d" {
		t.Fatalf("dir %q, detach %v, args %q", a.dir, a.detach, args)
	}
}

// TestShortHelp: the summary of each command fits a line of help, and the
// commands that a project starts with show examples.
func TestShortHelp(t *testing.T) {
	root, _ := Reference()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.HasParent() && len(c.Short) > 60 {
			t.Errorf("%s: summary of %d characters: %q", c.CommandPath(), len(c.Short), c.Short)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	for _, name := range []string{"init", "plan", "up", "destroy", "logs", "runs", "exec"} {
		if c, _, err := root.Find([]string{name}); err != nil || c.Example == "" {
			t.Errorf("%s has no examples", name)
		}
	}
}
