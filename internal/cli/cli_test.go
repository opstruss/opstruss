package cli

import (
	"runtime"
	"strings"
	"testing"
)

func noEnv(string) (string, bool) { return "", false }

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
		{args: []string{"--completion", "zsh"}, stdout: "#compdef opstruss"},
		{args: []string{"--completion", "csh"}, code: 1, stderr: "--completion takes bash, zsh, or fish"},
		{args: []string{"-C"}, code: 1, stderr: "-C needs a directory"},
		{args: []string{"-C", t.TempDir(), "validate"}, code: 1, stderr: "no opstruss.yaml in"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tt.args, &stdout, &stderr, noEnv); code != tt.code {
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

func TestTakeDir(t *testing.T) {
	a := &app{}
	args, err := a.takeDir([]string{"-C", "/srv", "-Cops", "explain", "--", "-C", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if a.dir != "/srv/ops" || strings.Join(args, " ") != "explain -- -C x" {
		t.Fatalf("dir %q, args %q", a.dir, args)
	}
}
