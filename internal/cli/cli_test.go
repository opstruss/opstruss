package cli

import (
	"runtime"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	platform := " " + runtime.GOOS + "/" + runtime.GOARCH + "\n"
	tests := []struct {
		args   []string
		code   int
		stdout func(string) bool
		stderr string
	}{
		{args: nil, stdout: isUsage},
		{args: []string{"help"}, stdout: isUsage},
		{args: []string{"-h"}, stdout: isUsage},
		{args: []string{"--help"}, stdout: isUsage},
		{
			args: []string{"version"},
			stdout: func(s string) bool {
				return strings.HasPrefix(s, "opstruss ") && strings.HasSuffix(s, platform)
			},
		},
		{args: []string{"version", "extra"}, code: 1, stderr: "opstruss: version takes no arguments\n"},
		{args: []string{"nope"}, code: 1, stderr: "opstruss: unknown command \"nope\"\n\n" + usage},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr strings.Builder
			if code := run(tt.args, &stdout, &stderr); code != tt.code {
				t.Errorf("exit code = %d, want %d", code, tt.code)
			}
			if tt.stdout == nil && stdout.Len() > 0 || tt.stdout != nil && !tt.stdout(stdout.String()) {
				t.Errorf("unexpected stdout %q", stdout.String())
			}
			if stderr.String() != tt.stderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.stderr)
			}
		})
	}
}

func isUsage(s string) bool { return s == usage }
