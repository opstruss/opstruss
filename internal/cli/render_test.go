package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/opstruss/opstruss/internal/engine"
)

// at gives an event a time, ms milliseconds after a fixed start.
func at(ev engine.Event, ms int) engine.Event {
	ev.Time = fmt.Sprintf("2026-10-25T10:00:00.%03dZ", ms)
	return ev
}

func renderAll(r *renderer, events ...engine.Event) (string, string) {
	var out, errs strings.Builder
	for _, ev := range events {
		for _, p := range r.render(ev) {
			if p.w == r.err {
				errs.WriteString(p.text)
			} else {
				out.WriteString(p.text)
			}
		}
	}
	for _, p := range r.end() {
		out.WriteString(p.text)
	}
	return out.String(), errs.String()
}

func terminalRenderer(color bool) *renderer {
	var out, errs strings.Builder
	return newRenderer(&out, &errs, false, true, false, color, color)
}

func ev(typ, addr, task, step, status, outcome, msg string) engine.Event {
	level := "info"
	if outcome == "failure" && status == "" {
		level = "error"
	}
	return engine.Event{Run: "r", Type: typ, Level: level, Address: addr, Task: task, Step: step, Status: status,
		Outcome: outcome, Message: msg}
}

func logLine(addr, task, step, msg string) engine.Event {
	return engine.Event{Run: "r", Type: "log", Level: "info", Address: addr, Task: task, Step: step, Stream: "stdout", Message: msg}
}

// Steps one after another are blocks: a line that starts each, the lines
// of its program under the gutter, and a line with how it ended and the
// time it took.
func TestRenderBlocks(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		at(ev("run", "", "build", "", "running", "", "started"), 0),
		at(ev("task", "", "build", "", "running", "", "started"), 1),
		at(ev("step", "", "build", "compile", "running", "", "started"), 2),
		at(logLine("", "build", "compile", "compiling"), 3),
		at(ev("step", "", "build", "compile", "finished", "success", "success"), 402),
		at(ev("step", "", "build", "lint", "finished", "skipped", "skipped"), 403),
		at(ev("step", "", "build", "test", "running", "", "started"), 404),
		at(logLine("", "build", "test", "\x1b[31mFAIL\x1b[0m"), 405),
		at(ev("step", "", "build", "test", "finished", "failure", "failed"), 905),
		at(ev("task", "", "build", "", "finished", "failure", "failed"), 906),
		at(ev("run", "", "build", "", "finished", "failure", "failure"), 907),
	)
	want := `▸ build
▸ build › compile
│ compiling
✓ build › compile  0.4s
○ build › lint
▸ build › test
│ FAIL
✗ build › test  0.5s
✗ build  0.9s
`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// While two operations print at once, each line of a program is led by its
// source; once one ends, the block of the other starts again.
func TestRenderParallel(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		at(ev("unit", "default/net", "up", "", "running", "", "up: started"), 0),
		at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 1),
		at(logLine("default/net", "up", "", "creating"), 2),
		at(logLine("default/web", "up", "", "added 312 packages"), 3),
		at(logLine("default/net", "up", "", "still creating"), 4),
		at(ev("unit", "default/web", "up", "", "finished", "success", "up: done"), 900),
		at(logLine("default/net", "up", "", "created"), 901),
		at(ev("unit", "default/net", "up", "", "finished", "failure", "the program exited with 1"), 950),
	)
	want := `▸ default/net › up
▸ default/web › up
▸ default/net › up
│ creating
default/web › up │ added 312 packages
default/net › up │ still creating
✓ default/web › up: done  0.9s
▸ default/net › up
│ created
✗ default/net › up: the program exited with 1  0.9s
`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// Services print without a start or an end: two of them lead each line
// with their names, padded to the widest.
func TestRenderServices(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		logLine("default/api", "", "", "listening"),
		logLine("default/worker-pool", "", "", "ready"),
		logLine("default/api", "", "", "GET /"),
	)
	want := `▸ default/api
│ listening
default/worker-pool │ ready
default/api         │ GET /
`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// In color, a program keeps its own colors and the lines of OpsTruss get
// theirs; without color there is no escape at all, and in either case
// nothing but color reaches the terminal.
func TestRenderColor(t *testing.T) {
	events := []engine.Event{
		at(ev("plan", "default/db", "plan", "", "finished", "success", "will go down"), 0),
		at(ev("unit", "default/db", "down", "", "running", "", "down: started"), 1),
		at(logLine("default/db", "down", "", "\x1b[32mgone\x1b[0m\x1b]0;title\x07 \x1b[2K"), 2),
		at(ev("unit", "default/db", "down", "", "finished", "success", "down: done"), 3),
		at(ev("diagnostic", "default/db", "", "", "", "", "a \x1b]52;c;Zm9v\x07warning"), 4),
	}
	events[0].Plan = map[string]any{"changes": []any{map[string]any{"action": "delete"}}}
	events[4].Level = "warn"
	out, errs := renderAll(terminalRenderer(false), events...)
	if strings.Contains(out+errs, "\x1b") || !strings.Contains(out, "│ gone \n") || errs != "default/db: a warning\n" {
		t.Errorf("without color:\n%q\n%q", out, errs)
	}
	out, errs = renderAll(terminalRenderer(true), events...)
	for _, s := range []string{"\x1b[31m-\x1b[0m default/db  will go down", "\x1b[32mgone\x1b[0m", "\x1b[32m✓\x1b[0m default/db › down: done"} {
		if !strings.Contains(out, s) {
			t.Errorf("in color, no %q in\n%q", s, out)
		}
	}
	if strings.Contains(out+errs, "title") || strings.Contains(out+errs, "\x1b]") || strings.Contains(out, "\x1b[2K") {
		t.Errorf("an escape other than color: %q %q", out, errs)
	}
}

// Events of an older OpsTruss, without a status, still render.
func TestRenderOldEvents(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		ev("plan", "default/web", "", "", "", "", "will run: new"),
		logLine("default/web", "up", "", "hello"),
		ev("unit", "default/web", "", "", "", "", "up: done"),
	)
	want := "= default/web  will run: new\n▸ default/web › up\n│ hello\ndefault/web: up: done\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// Without a terminal, a line per event as before: the events that only
// pair a group are left out, and so is color, unless it is on.
func TestRenderLines(t *testing.T) {
	events := []engine.Event{
		ev("run", "", "build", "", "running", "", "started"),
		ev("task", "", "build", "", "running", "", "started"),
		ev("step", "", "build", "compile", "running", "", "started"),
		logLine("", "build", "compile", "\x1b[1mcompiling\x1b[0m\r"),
		ev("step", "", "build", "compile", "finished", "success", "success"),
		ev("step", "", "build", "lint", "finished", "skipped", "skipped"),
		ev("task", "default/app", "default/app/backup", "", "finished", "success", "success"),
		ev("task", "", "build", "", "finished", "success", "success"),
	}
	var out, errs strings.Builder
	want := "build: started\nbuild: compile | compiling\nbuild: lint: skipped\ndefault/app/backup: success\nbuild: success\n"
	got, _ := renderAll(newRenderer(&out, &errs, false, false, false, false, false), events...)
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	got, _ = renderAll(newRenderer(&out, &errs, false, false, false, true, true), events...)
	if !strings.Contains(got, "build: compile | \x1b[1mcompiling\x1b[0m\n") {
		t.Errorf("in color: %q", got)
	}
}

// In GitHub Actions, the lines of each source are a group, which any other
// line closes, so a line of OpsTruss stays outside; a line of a program
// keeps its prefix, so it never starts with :: for the runner to read.
func TestRenderGitHubGroups(t *testing.T) {
	var out, errs strings.Builder
	r := newRenderer(&out, &errs, false, false, true, false, false)
	got, gotErr := renderAll(r,
		ev("unit", "default/app", "up", "", "running", "", "up: started"),
		logLine("default/app", "up", "", "one"),
		logLine("default/app", "up", "", "::warning::two"),
		logLine("default/db", "up", "", "three"),
		ev("unit", "default/db", "up", "", "", "failure", "the program exited with 1"),
		logLine("default/app", "up", "", "four"),
	)
	want := `::group::default/app › up
default/app | one
default/app | ::warning::two
::endgroup::
::group::default/db › up
default/db | three
::endgroup::
::group::default/app › up
default/app | four
::endgroup::
`
	if got != want || gotErr != "default/db: the program exited with 1\n" {
		t.Errorf("got\n%s\nwant\n%s\nstderr %q", got, want, gotErr)
	}
}
