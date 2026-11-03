package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// held is a program that, while hold-<name> exists, waits for the signal
// that stops it and writes which one came to got; otherwise it ends.
const held = `test -f hold-$0 || exit 0; for s in HUP INT TERM USR1; do trap "echo $0: $s >> got; exit 0" $s; done; touch ready-$0; while :; do sleep 0.05; done`

func heldRun(name string) string { return `[sh, -c, '` + held + `', "` + name + `"]` }

// interrupt runs op until each program in names waits for its signal, then
// cancels the run, as Ctrl-C does.
func interrupt(t *testing.T, dir string, op func(context.Context) (*Result, error), names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, "hold-"+n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var res *Result
	var err error
	go func() {
		defer close(done)
		res, err = op(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for _, n := range names {
		for !fileExists(filepath.Join(dir, "ready-"+n)) {
			select {
			case <-done:
				t.Fatalf("the run ended before %s started: %v, %+v", n, err, res)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not start", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end")
	}
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, "hold-"+n)); err != nil {
			t.Fatal(err)
		}
	}
}

// Each key of the stop of a program comes from the nearest that writes it:
// a unit over every program run for it, the plan, status, and destroy of
// its plugin included, and over the units of a component it uses; a task
// over the programs of its steps and the tasks they call; and both over
// the stop of a task in a plugin's manifest.
func TestStopWhere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	units := `name: where
uses:
  p: ./p
units:
  plugged:
    uses: p
    with: {name: plugged}
  graced:
    uses: p
    with: {name: graced}
    stop: {grace: 30s}
  watched:
    uses: p
    with: {name: watched}
    stop: {signal: SIGUSR1}
`
	tasks := `tasks:
  steps:
    steps:
      - run: ` + heldRun("unit-step") + `
  one:
    stop: {signal: SIGTERM}
    p.show: {name: one-call}
  calls:
    stop: {signal: SIGHUP}
    steps:
      - p.show: {name: step-call}
`
	project := writeProject(t, map[string]string{
		"opstruss.yaml": units + tasks,
		"p/opstruss.plugin.yaml": `name: p
inputs:
  name: {type: string, default: x}
tasks:
  plan:
    run: ` + heldRun("plan-${{ inputs.name }}") + `
  up:
    stop: {signal: SIGTERM}
    run: ` + heldRun("up-${{ inputs.name }}") + `
  destroy:
    stop: {signal: SIGTERM}
    run: ` + heldRun("destroy-${{ inputs.name }}") + `
  status:
    run: ` + heldRun("status-${{ inputs.name }}") + `
  show:
    stop: {signal: SIGUSR1}
    run: ` + heldRun("${{ inputs.name }}") + `
`,
		// A unit of a component runs in the component's directory.
		"site/opstruss.yaml": "units:\n  inner:\n    run: [sh, -c, 'cd .. && " + held + "', inner]\n    stop: {grace: 10s}\n",
	})
	s := &screen{}
	opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit, Yes: true}
	if res, err := Up(t.Context(), opts); err != nil || res.ExitCode != 0 {
		t.Fatalf("up: %v, %+v; %q", err, res, s.seen)
	}
	var rec struct {
		Destroy struct {
			Stop map[string]string `json:"stop"`
		} `json:"destroy"`
	}
	data, err := os.ReadFile(filepath.Join(project, ".opstruss", "state", "default", "graced", "state.json"))
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil || rec.Destroy.Stop["signal"] != "SIGTERM" || rec.Destroy.Stop["grace"] != "30s" {
		t.Errorf("destroy contract of graced: %v, %s", err, data)
	}
	interrupt(t, project, func(ctx context.Context) (*Result, error) { return PlanContext(ctx, opts) }, "plan-plugged")
	// status runs the units one at a time.
	for _, program := range []string{"status-plugged", "status-watched"} {
		interrupt(t, project, func(ctx context.Context) (*Result, error) { return StatusUnits(ctx, opts) }, program)
	}
	interrupt(t, project, func(ctx context.Context) (*Result, error) { return Task(ctx, opts, "one", nil) }, "one-call")
	interrupt(t, project, func(ctx context.Context) (*Result, error) { return Task(ctx, opts, "calls", nil) }, "step-call")
	for _, add := range [][2]string{{"  tasked:\n    uses: steps\n    stop: {signal: SIGTERM}\n", "unit-step"},
		{"  site:\n    uses: ./site\n    stop: {signal: SIGTERM}\n", "inner"}} {
		units += add[0]
		if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(units+tasks), 0o600); err != nil {
			t.Fatal(err)
		}
		interrupt(t, project, func(ctx context.Context) (*Result, error) { return Up(ctx, opts) }, add[1])
	}
	interrupt(t, project, func(ctx context.Context) (*Result, error) { return Destroy(ctx, opts) }, "destroy-plugged", "destroy-graced")
	got, _ := os.ReadFile(filepath.Join(project, "got"))
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := []string{"plan-plugged: INT", "status-plugged: INT", "status-watched: USR1", "one-call: TERM", "step-call: HUP",
		"unit-step: TERM", "inner: TERM", "destroy-plugged: TERM", "destroy-graced: TERM"}
	slices.Sort(lines)
	slices.Sort(want)
	if !slices.Equal(lines, want) {
		t.Errorf("got %q, want %q", lines, want)
	}
}

// always: after a cancel has the stop.grace of its task, not the default.
func TestStopAlways(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := writeProject(t, map[string]string{"opstruss.yaml": `name: always
tasks:
  clean:
    stop: {grace: 300ms}
    steps:
      - block:
          - run: ` + heldRun("work") + `
        always:
          - run: sleep 2; echo always >> got
`})
	opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Yes: true}
	interrupt(t, project, func(ctx context.Context) (*Result, error) { return Task(ctx, opts, "clean", nil) }, "work")
	if got, _ := os.ReadFile(filepath.Join(project, "got")); string(got) != "work: INT\n" {
		t.Errorf("got %q", got)
	}
}
