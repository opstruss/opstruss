package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/opstruss/opstruss/internal/model"
)

// TestQuestionOrder: the questions of a run come in the order of its plan,
// also when the plan of the first unit takes longer at its turn.
func TestQuestionOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	files := map[string]string{
		"opstruss.yaml":             "name: order\nuses:\n  slow: ./slow\nunits:\n  a:\n    uses: slow\n  b:\n    run: [echo, b]\n  c:\n    run: [echo, c]\n",
		"slow/opstruss.plugin.yaml": "name: slow\ntasks:\n  plan:\n    run: [./plan.sh]\n  up:\n    run: [\"true\"]\n",
		"slow/plan.sh": "#!/bin/sh\nsleep 0.2\n" +
			`echo '{"changes": [{"action": "create", "resource": "a"}], "digest": "sha256:a", "exact": true}' > "$OPSTRUSS_PLAN"` + "\n",
	}
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, name), []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		var mu sync.Mutex
		var asked []string
		_, err := Up(t.Context(), Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv,
			Approve: func(_ context.Context, q Question) (bool, error) {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, q.Address)
				return false, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"default/a", "default/b", "default/c"}; !slices.Equal(asked, want) {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// TestRerunKeepsAnswers: an input answered at the terminal is given again
// to runs rerun, as a flag is.
func TestRerunKeepsAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	project := t.TempDir()
	yaml := "name: greet\ntasks:\n  greet:\n    inputs:\n      name: {type: string, required: true}\n    run: echo \"hello $INPUT_NAME\"\n"
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	emit := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, ev.Message)
	}
	opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: emit,
		Ask: func(context.Context, string, *model.Input) (string, error) { return "Sari", nil }}
	res, err := Task(t.Context(), opts, "greet", nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("task: %v, %+v", err, res)
	}
	opts.Ask = nil
	res, err = Rerun(t.Context(), opts, res.Run)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("rerun: %v, %+v; %q", err, res, lines)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(lines, "hello Sari") || !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "rerun of run ") }) {
		t.Fatalf("lines %q", lines)
	}
	if n := slices.Index(lines, "hello Sari"); slices.Index(lines[n+1:], "hello Sari") < 0 {
		t.Fatalf("the rerun did not greet Sari: %q", lines)
	}
}
