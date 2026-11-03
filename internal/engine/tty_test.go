package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// screen is a Terminal for a test: it records what Draw shows and the
// events of the run, in order.
type screen struct {
	mu   sync.Mutex
	seen []string // "draw <rows joined by |>", or "event <type> <message>"
	logs []string // the messages of the log events of programs
}

func (s *screen) terminal() *Terminal {
	return &Terminal{
		Size: func() (int, int) { return 82, 26 },
		Draw: func(r Rows) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.seen = append(s.seen, "draw "+strings.Join(r.Rows, "|"))
		},
	}
}

func (s *screen) emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, "event "+ev.Type+" "+ev.Message)
	if ev.Type == "log" && ev.Stream != "" {
		s.logs = append(s.logs, ev.Message)
	}
}

func (s *screen) draws() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.seen {
		if strings.HasPrefix(x, "draw ") {
			n++
		}
	}
	return n
}

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A program with tty draws its rows as it goes, and its log has the rows
// that the terminal shows at the end, not each frame.
func TestTTYTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := writeProject(t, map[string]string{"opstruss.yaml": `name: frames
tasks:
  show:
    tty: true
    run: |
      printf 'a\nb\n'
      sleep 0.3
      printf '\033[2Aa2\nb2\n'
`})
	s := &screen{}
	res, err := Task(t.Context(), Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit, Terminal: s.terminal()}, "show", nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("task: %v, %+v", err, res)
	}
	if !slices.Equal(s.logs, []string{"a2", "b2"}) {
		t.Errorf("logs %q", s.logs)
	}
	if !slices.Contains(s.seen, "draw a|b") {
		t.Errorf("no draw of the first frame: %q", s.seen)
	}
	// The rows go from the terminal before the last rows and the end of the
	// task come as events.
	at := slices.Index(s.seen, "draw ")
	end := slices.IndexFunc(s.seen, func(x string) bool { return strings.HasPrefix(x, "event task ") && x != "event task started" })
	if at < 0 || end < at || slices.Index(s.seen, "event log a2") < at {
		t.Errorf("order %q", s.seen)
	}
}

// A program gets pipes without tty, without a terminal, and when an output
// reads its stdout.
func TestTTYPipes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := writeProject(t, map[string]string{"opstruss.yaml": `name: pipes
tasks:
  plain:
    run: printf 'a\n\033[1Ab\n'
  off:
    tty: false
    run: printf 'a\n\033[1Ab\n'
  read:
    tty: true
    run: echo out=yes
    outputs:
      out: {from: stdout, format: dotenv}
`})
	for _, c := range []struct {
		task     string
		terminal bool
		logs     []string
	}{
		{"plain", true, []string{"a", "b"}},
		{"off", true, []string{"a", "b"}},
		{"read", true, []string{"captured stdout as outputs.out (8 B, dotenv)"}},
	} {
		s := &screen{}
		opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit}
		if c.terminal {
			opts.Terminal = s.terminal()
		}
		res, err := Task(t.Context(), opts, c.task, nil)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %v, %+v", c.task, err, res)
		}
		if !slices.Equal(s.logs, c.logs) || s.draws() > 0 {
			t.Errorf("%s: logs %q, %d draws", c.task, s.logs, s.draws())
		}
	}

	// tty without a terminal is a pipe too.
	s := &screen{}
	project = writeProject(t, map[string]string{"opstruss.yaml": "name: pipes\ntasks:\n  on:\n    tty: true\n    run: printf 'a\\n\\033[1Ab\\n'\n"})
	if _, err := Task(t.Context(), Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit}, "on", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.logs, []string{"a", "b"}) {
		t.Errorf("without a terminal: logs %q", s.logs)
	}
}

// The program nearest to tty decides: a step over its task, a block over
// its task, a step that calls a task of a plugin over its manifest, a unit
// over the task it uses, and a unit inside a component over the unit that
// uses the component. A secret comes through pipes, whatever tty says.
func TestTTYWhere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	is := `if test -t 1; then echo "$0: tty"; else echo "$0: pipe"; fi`
	project := writeProject(t, map[string]string{
		"opstruss.yaml": `name: where
uses:
  p: ./p
secrets:
  token: {p.token: {}}
units:
  plugin-unit:
    uses: p.show
    with: {name: plugin-unit}
    tty: false
  site:
    uses: ./site
    tty: true
tasks:
  all:
    tty: true
    steps:
      - run: [sh, -c, '` + is + `', task-default]
      - run: [sh, -c, '` + is + `', step-off]
        tty: false
      - block:
          - run: [sh, -c, '` + is + `', block-off]
          - run: [sh, -c, '` + is + `', block-step-on]
            tty: true
        tty: false
      - p.show: {name: manifest}
      - p.show: {name: step-off-over-manifest}
        tty: false
      - run: [sh, -c, 'echo "secret: ${#TOKEN}"']
        env: {TOKEN: "${{ secrets.token }}"}
`,
		"p/opstruss.plugin.yaml": `name: p
inputs:
  name: {type: string, default: x}
tasks:
  show:
    tty: true
    run: [sh, -c, '` + is + `', "${{ inputs.name }}"]
  token:
    tty: true
    outputs:
      value: {sensitive: true}
    run: |
      test -t 1 && echo terminal
      echo "value=a-long-token" >> "$OPSTRUSS_OUTPUT"
`,
		"site/opstruss.yaml": `units:
  inside-default:
    run: [sh, -c, '` + is + `', inside-default]
  inside-off:
    run: [sh, -c, '` + is + `', inside-off]
    tty: false
`,
	})
	s := &screen{}
	opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit, Terminal: s.terminal(), Yes: true}
	if res, err := Task(t.Context(), opts, "all", nil); err != nil || res.ExitCode != 0 {
		t.Fatalf("task: %v, %+v; %q", err, res, s.logs)
	}
	if res, err := Up(t.Context(), opts); err != nil || res.ExitCode != 0 {
		t.Fatalf("up: %v, %+v; %q", err, res, s.logs)
	}
	want := []string{"task-default: tty", "step-off: pipe", "block-off: pipe", "block-step-on: tty", "manifest: tty",
		"step-off-over-manifest: pipe", "secret: 12", "plugin-unit: pipe", "inside-default: tty", "inside-off: pipe"}
	var got []string
	for _, l := range s.logs {
		if strings.Contains(l, ": ") {
			got = append(got, l)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || slices.Contains(s.logs, "terminal") {
		t.Errorf("logs %q, want %q", s.logs, want)
	}
}

// tty is not part of what a unit is: a unit whose tty alone changed is
// unchanged, and its destroy contract keeps the new tty without running
// up, which destroy reads once the unit has left the files.
func TestTTYContract(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	is := `if test -t 1; then echo "$0: tty"; else echo "$0: pipe"; fi`
	unit := func(tty string) string {
		return "name: contract\nunits:\n  r:\n    run: [sh, -c, 'echo run >> runs']\n    destroy: [sh, -c, '" + is + "', destroy]\n" + tty
	}
	project := writeProject(t, map[string]string{"opstruss.yaml": unit("")})
	s := &screen{}
	opts := Options{Dir: project, Environ: os.Environ(), LookupEnv: os.LookupEnv, Emit: s.emit, Terminal: s.terminal(), Yes: true}
	for _, yaml := range []string{unit(""), unit("    tty: true\n")} {
		if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if res, err := Up(t.Context(), opts); err != nil || res.ExitCode != 0 {
			t.Fatalf("up: %v, %+v", err, res)
		}
	}
	if runs, _ := os.ReadFile(filepath.Join(project, "runs")); string(runs) != "run\n" {
		t.Errorf("up ran %q", runs)
	}
	if !slices.Contains(s.seen, "event unit unchanged") {
		t.Errorf("events %q", s.seen)
	}
	rec, err := os.ReadFile(filepath.Join(project, ".opstruss", "state", "default", "r", "state.json"))
	if err != nil || !strings.Contains(string(rec), `"tty": true`) {
		t.Fatalf("record %s: %v", rec, err)
	}
	removed := "name: contract\nremoved:\n  - {from: r, destroy: true}\n"
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(removed), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := Up(t.Context(), opts); err != nil || res.ExitCode != 0 {
		t.Fatalf("up: %v, %+v", err, res)
	}
	if !slices.Contains(s.logs, "destroy: tty") {
		t.Errorf("logs %q", s.logs)
	}
}
