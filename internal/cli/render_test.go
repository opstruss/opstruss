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

// Steps one after another are blocks: a line that starts each with its
// first line, the lines of its program under the gutter, and a line with
// how it ended and the time it took; a task that prints nothing of its own
// starts no block.
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
	want := `▸ build › compile
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

// While two operations print at once, the block that printed first shows
// its lines as they come, and the other holds its lines and shows whole
// once the first ends, as turbo run --log-order=grouped does. Units are
// named without their stack.
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
	want := `▸ net › up
│ creating
│ still creating
│ created
✗ net › up: the program exited with 1  0.9s
▸ web › up
│ added 312 packages
✓ web › up: done  0.9s
`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// Services print without a start or an end: two of them lead each line
// with their names, padded to the widest. What OpsTruss says of a unit
// while it goes up, as that its service started, is a line of its block.
func TestRenderServices(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		at(ev("unit", "default/api", "up", "", "running", "", "up: started"), 0),
		at(ev("unit", "default/api", "", "", "", "", "started, pid 7"), 1),
		at(ev("unit", "default/api", "up", "", "finished", "success", "up: done"), 100),
		logLine("default/api", "", "", "listening"),
		logLine("default/worker-pool", "", "", "ready"),
		logLine("default/api", "", "", "GET /"),
	)
	want := `▸ api › up
│ started, pid 7
✓ api › up: done  0.1s
▸ api
│ listening
worker-pool │ ready
api         │ GET /
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
		at(ev("plan", "default/db", "plan", "", "finished", "success", "will be destroyed"), 0),
		at(ev("unit", "default/db", "destroy", "", "running", "", "destroy: started"), 1),
		at(logLine("default/db", "destroy", "", "\x1b[32mgone\x1b[0m\x1b]0;title\x07 \x1b[2K"), 2),
		at(ev("unit", "default/db", "destroy", "", "finished", "success", "destroy: done"), 3),
		at(ev("diagnostic", "default/db", "", "", "", "", "a \x1b]52;c;Zm9v\x07warning"), 4),
	}
	events[0].Plan = map[string]any{"changes": []any{map[string]any{"action": "delete"}}}
	events[4].Level = "warn"
	out, errs := renderAll(terminalRenderer(false), events...)
	if strings.Contains(out+errs, "\x1b") || !strings.Contains(out, "│ gone \n") || errs != "db: a warning\n" {
		t.Errorf("without color:\n%q\n%q", out, errs)
	}
	out, errs = renderAll(terminalRenderer(true), events...)
	for _, s := range []string{"\x1b[31m-\x1b[0m db  destroy", "\x1b[32mgone\x1b[0m", "\x1b[32m✓\x1b[0m db › destroy: done"} {
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
	want := "  + web  new\n\nPlan of stack default: 1 to run\n\n▸ web › up\n│ hello\nweb: up: done\n"
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
	want := `::group::app › up
default/app | one
default/app | ::warning::two
::endgroup::
::group::db › up
default/db | three
::endgroup::
::group::app › up
default/app | four
::endgroup::
`
	if got != want || gotErr != "default/db: the program exited with 1\n" {
		t.Errorf("got\n%s\nwant\n%s\nstderr %q", got, want, gotErr)
	}
}

func planEvent(addr, outcome, msg string, plan map[string]any) engine.Event {
	e := ev("plan", addr, "plan", "", "finished", outcome, msg)
	e.Plan = plan
	return e
}

// The plan lines of the units wait until the plan is done, then show as
// one table: the changes first, then a unit that only a warning names,
// then those that cannot be planned, each hint on a line of its own, and a
// line that counts them, also the units that do not change, which the
// table leaves out. The block of the plan of a plugin, without its blank
// first line, ends before it.
func TestRenderPlanTable(t *testing.T) {
	created := map[string]any{"changes": []any{map[string]any{"action": "create", "resource": "terraform_data.greeting"}}}
	waits := map[string]any{"changes": []any{}, "after": "default/qa-out", "reason": "new"}
	may := map[string]any{"changes": []any{}, "after": "default/infra"}
	auto := map[string]any{"changes": []any{map[string]any{"action": "run"}}, "approval": "auto"}
	orphan := ev("diagnostic", "default/qa-http", "", "", "", "", "orphan: its unit is no longer in the files; add {from: qa-http, destroy: true} to removed: to destroy it, or destroy: false to forget it")
	orphan.Level = "warn"
	out, _ := renderAll(terminalRenderer(false),
		at(ev("unit", "default/infra", "plan", "", "running", "", "plan: started"), 0),
		at(logLine("default/infra", "plan", "", ""), 1),
		at(logLine("default/infra", "plan", "", "OpenTofu will perform the following actions:"), 2),
		at(planEvent("default/infra", "success", "will run: new; plan: create terraform_data.greeting", created), 400),
		planEvent("default/qa-build", "success", "will run: new", auto),
		planEvent("default/tesunit", "success", "unchanged", map[string]any{"changes": []any{}}),
		planEvent("default/qa-each[a]", "failure", "error: this address was forgotten in run R; opstruss state rm qa-each[a] removes the tombstone so the name can be used again", nil),
		planEvent("default/qa-stdin", "success", "will run: new; plan after default/qa-out (stdin)", waits),
		planEvent("default/web", "success", "may run: plan after default/infra (env.GREETING)", may),
		planEvent("default/qa-secret", "failure", "error: env.TOKEN: secret qa_token: QA_TOKEN is not set", nil),
		orphan,
		ev("unit", "default/qa-build", "up", "", "running", "", "up: started"),
		logLine("default/qa-build", "up", "", "built"),
	)
	want := `▸ infra › plan
│ OpenTofu will perform the following actions:
✓ infra › plan  0.4s

  + infra       new · create terraform_data.greeting
  + qa-build    new · approval: auto
  + qa-stdin    new · after qa-out (stdin)
  ? web         after infra (env.GREETING)
  ! qa-http     orphan: its unit is no longer in the files
                → add {from: qa-http, destroy: true} to removed: to destroy it, or destroy: false to forget it
  ✗ qa-each[a]  this address was forgotten in run R
                → opstruss state rm qa-each[a] removes the tombstone so the name can be used again
  ✗ qa-secret   env.TOKEN: secret qa_token: QA_TOKEN is not set

Plan of stack default: 3 to run, 1 may run, 1 unchanged, 1 orphan, 2 cannot be planned

▸ qa-build › up
│ built
`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// A note of the run that comes while the plan is gathered, as that this
// machine got an age key, comes under the table, not between the blocks of
// the plans of plugins.
func TestRenderNotesAfterPlan(t *testing.T) {
	run := map[string]any{"changes": []any{map[string]any{"action": "run"}}}
	out, _ := renderAll(terminalRenderer(false),
		planEvent("default/web", "success", "will run: new", run),
		ev("diagnostic", "", "", "", "", "", "made the age key age1x of this machine in keys.txt; back it up"),
		planEvent("default/db", "success", "will run: new", run),
		ev("unit", "default/web", "up", "", "running", "", "up: started"),
	)
	want := "  + web  new\n  + db   new\n\nPlan of stack default: 2 to run\n\nmade the age key age1x of this machine in keys.txt; back it up\n\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// Each line of a message is a line of its block at a terminal, and led by
// its source without one, so a line of a log step never reads as a
// workflow command in GitHub Actions; the line end that closes the message
// adds no blank line.
func TestRenderMultiline(t *testing.T) {
	events := []engine.Event{
		at(ev("step", "", "notify", "say", "running", "", "started"), 0),
		at(logLine("", "notify", "say", "first\n::error::second\n"), 1),
		at(ev("step", "", "notify", "say", "finished", "success", "success"), 2),
	}
	out, _ := renderAll(terminalRenderer(false), events...)
	if want := "▸ notify › say\n│ first\n│ ::error::second\n✓ notify › say  0.0s\n"; out != want {
		t.Errorf("at a terminal, got\n%s\nwant\n%s", out, want)
	}
	var o, e strings.Builder
	out, _ = renderAll(newRenderer(&o, &e, false, false, true, false, false), events...)
	if want := "::group::notify › say\nnotify: say | first\nnotify: say | ::error::second\n::endgroup::\n"; out != want {
		t.Errorf("in GitHub Actions, got\n%s\nwant\n%s", out, want)
	}
}

func statusEvent(addr, msg string, outputs map[string]any) engine.Event {
	e := ev("unit", addr, "", "", "", "", msg)
	e.Outputs = outputs
	return e
}

// The status of the units is a table too: what the record of each says,
// also of a unit whose plugin printed its status above, with a status of a
// plugin that failed as a warning under its unit.
func TestRenderStatusTable(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		at(ev("task", "default/infra", "default/infra/status", "", "running", "", "started"), 0),
		at(logLine("default/infra", "default/infra/status", "", "terraform_data.greeting"), 1),
		at(ev("task", "default/infra", "default/infra/status", "", "finished", "success", "success"), 400),
		statusEvent("default/infra", "up, from run R1 at 2026-10-25T12:58:33Z",
			map[string]any{"state": "up", "run": "R1", "updated": "2026-10-25T12:58:33Z"}),
		ev("unit", "default/infra", "", "", "", "", `outputs: {"greeting":"hi"}`),
		at(ev("task", "default/web", "default/web/status", "", "running", "", "started"), 500),
		at(ev("task", "default/web", "default/web/status", "", "finished", "failure", "failed"), 600),
		statusEvent("default/web", "pending: up of run R1 failed", map[string]any{"state": "pending", "run": "R1", "updated": ""}),
		statusEvent("default/db", "absent", map[string]any{"state": "absent", "run": "", "updated": ""}),
	)
	want := `▸ infra › status
│ terraform_data.greeting
✓ infra › status  0.4s
✗ web › status  0.1s

  ● infra  up · 2026-10-25 12:58 UTC · run R1
           outputs: {"greeting":"hi"}
  ! web    pending: up of run R1 failed
           ! the status of its plugin failed, above
  ○ db     absent

Status of stack default: 1 up, 1 pending, 1 absent

`
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// A unit that an interrupt stopped ends cancelled; one that it kept from
// starting, also after a unit it needs, shows no line of its own, and the
// summary counts both as cancelled.
func TestRenderInterrupted(t *testing.T) {
	out, _ := renderAll(terminalRenderer(false),
		at(ev("unit", "default/net", "up", "", "running", "", "up: started"), 0),
		at(logLine("default/net", "up", "", "creating"), 1),
		at(ev("unit", "default/net", "up", "", "finished", "cancelled", "interrupted: the program exited with 130"), 500),
		at(ev("unit", "default/web", "up", "", "finished", "skipped", "skipped: default/net is cancelled"), 501),
		at(ev("unit", "default/db", "up", "", "finished", "cancelled", "interrupted before its turn"), 502),
		at(ev("run", "", "", "", "finished", "cancelled", "cancelled"), 503),
	)
	want := "▸ net › up\n│ creating\n■ net › up: interrupted: the program exited with 130  0.5s\n\nUp of stack default: 3 cancelled\n"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}
