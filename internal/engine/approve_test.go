package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
				for _, it := range q.Items {
					asked = append(asked, it.Address)
				}
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

// TestAskFirst: a run that asks at the terminal asks once about every unit
// whose plan is known before any program runs, also one with approval:
// auto, so no output comes between the questions, and about a unit with no
// program that plans it that waits for the outputs of another; a plugin
// that plans such a unit asks at its turn.
func TestAskFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	yaml := `name: first
units:
  a:
    run: [touch, a-ran]
  b:
    run: [sh, -c, 'touch b-ran; printf "{\"n\": 1}" > "$OPSTRUSS_OUTPUT"']
    outputs:
      n: {type: number}
    approval: auto
  c:
    run: [touch, c-ran]
    env:
      N: ${{ units.b.outputs.n }}
  d:
    uses: ./planned
    with:
      n: ${{ units.b.outputs.n }}
`
	files := map[string]string{
		"opstruss.yaml":                yaml,
		"planned/opstruss.plugin.yaml": "name: planned\ninputs:\n  n: {type: number}\ntasks:\n  plan:\n    run: [./plan.sh]\n  up:\n    run: [\"true\"]\n",
		"planned/plan.sh": "#!/bin/sh\n" +
			`echo '{"changes": [{"action": "create", "resource": "d"}], "digest": "sha256:d", "exact": true}' > "$OPSTRUSS_PLAN"` + "\n",
	}
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(project, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, name), []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var asked []string
	res, err := Up(t.Context(), Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv,
		Approve: func(_ context.Context, q Question) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			_, err := os.Stat(filepath.Join(project, "b-ran"))
			who := q.Address
			for _, it := range q.Items {
				who = strings.TrimPrefix(who+","+it.Address, ",")
			}
			asked = append(asked, who+" after b: "+strconv.FormatBool(err == nil))
			return true, nil
		}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("up: %v, %+v", err, res)
	}
	if want := []string{"default/a,default/c after b: false", "default/d after b: true"}; !slices.Equal(asked, want) {
		t.Fatalf("asked %q, want %q", asked, want)
	}
}

// TestInterruptAtQuestion: a run that a signal stops while it asks ends
// cancelled, by signal, with the exit code that a shell reports.
func TestInterruptAtQuestion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	yaml := "name: ask\nunits:\n  a:\n    run: [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	res, err := Up(ctx, Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv,
		Approve: func(ctx context.Context, _ Question) (bool, error) {
			cancel(Interrupted{Signal: syscall.SIGINT})
			<-ctx.Done()
			return false, context.Cause(ctx)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != cancelled || res.ExitCode != 130 || res.Record.Reason != "signal" {
		t.Fatalf("got %s, exit %d, reason %q; want cancelled, 130, signal", res.Outcome, res.ExitCode, res.Record.Reason)
	}
}
