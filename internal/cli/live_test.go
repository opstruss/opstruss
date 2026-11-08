package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/opstruss/opstruss/internal/ansi"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"golang.org/x/sys/unix"
)

func rows(addr, task string, ms int, rows ...string) engine.Rows {
	return engine.Rows{Address: addr, Task: task, Time: eventTime(at(engine.Event{}, ms)), Rows: rows}
}

// renderSome renders events as renderAll does, without the end of the
// command.
func renderSome(r *renderer, events ...engine.Event) string {
	var out strings.Builder
	for _, ev := range events {
		for _, p := range r.render(ev) {
			out.WriteString(p.text)
		}
	}
	return out.String()
}

// The rows that a program may still change show in the area at the bottom
// of the terminal, under the gutter of its block: after its header until a
// line of the block is on the terminal, without it after.
func TestRenderLive(t *testing.T) {
	r := terminalRenderer(false)
	renderSome(r, at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 0))
	r.live(rows("default/web", "up", 1, "", "[+] up 0/1", " ⠋ Network Creating"))
	if want := "▸ web › up\n│ [+] up 0/1\n│  ⠋ Network Creating\n"; r.area() != want {
		t.Errorf("area %q, want %q", r.area(), want)
	}
	out := renderSome(r, at(logLine("default/web", "up", "", "[+] up 1/1"), 2))
	r.live(rows("default/web", "up", 3, " ✔ Network Created", "", "next"))
	if want := "│  ✔ Network Created\n│\n│ next\n"; out != "▸ web › up\n│ [+] up 1/1\n" || r.area() != want {
		t.Errorf("out %q, area %q, want %q", out, r.area(), want)
	}
	// Another source prints: the header comes again over the area.
	renderSome(r, at(ev("unit", "default/other", "", "", "", "", "a note"), 4))
	if !strings.HasPrefix(r.area(), "▸ web › up\n") {
		t.Errorf("area %q", r.area())
	}
	// No rows take the area away; so does the end of the block.
	r.live(rows("default/web", "up", 5))
	if r.area() != "" {
		t.Errorf("area %q", r.area())
	}
	r.live(rows("default/web", "up", 6, "again"))
	renderSome(r, at(ev("unit", "default/web", "up", "", "finished", "success", "up: done"), 7))
	if r.area() != "" {
		t.Errorf("area after the end %q", r.area())
	}
}

// Each program that draws has its rows in the area, under the header of
// its block; the lines of another block print above them as they come, and
// a block that shows holds the lines of the others as before.
func TestRenderLiveBlocks(t *testing.T) {
	r := terminalRenderer(true)
	renderSome(r,
		at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 0),
		at(ev("unit", "default/db", "up", "", "running", "", "up: started"), 1),
		at(ev("unit", "default/ticker", "up", "", "running", "", "up: started"), 2))
	r.live(rows("default/web", "up", 3, "\x1b[0;32m✔\x1b[0m web"))
	r.live(rows("default/db", "up", 4, "db row"))
	if a := stripAll(r.area()); a != "▸ web › up\n│ ✔ web\n▸ db › up\n│ db row\n" {
		t.Errorf("area %q", a)
	}
	if a := r.area(); !strings.Contains(a, "\x1b[0;32m✔\x1b[0m web") {
		t.Errorf("area %q has no color of its rows", a)
	}
	out := renderSome(r, at(logLine("default/ticker", "up", "", "tick 1"), 5))
	if stripAll(out) != "▸ ticker › up\n│ tick 1\n" || !strings.HasPrefix(stripAll(r.area()), "▸ web › up\n") {
		t.Errorf("out %q, area %q", out, r.area())
	}
	// web ends while ticker shows: its rows go, and its lines wait.
	r.live(rows("default/web", "up", 6))
	out = renderSome(r,
		at(logLine("default/web", "up", "", "web done"), 7),
		at(ev("unit", "default/web", "up", "", "finished", "success", "up: done"), 8))
	if out != "" || stripAll(r.area()) != "▸ db › up\n│ db row\n" {
		t.Errorf("out %q, area %q", out, r.area())
	}
	out = renderSome(r, at(ev("unit", "default/ticker", "up", "", "finished", "success", "up: done"), 9))
	if want := "✓ ticker › up: done  0.0s\n▸ web › up\n│ web done\n✓ web › up: done  0.0s\n"; stripAll(out) != want {
		t.Errorf("got\n%s\nwant\n%s", stripAll(out), want)
	}
}

// Without color the area has none, also of the rows.
func TestRenderLiveNoColor(t *testing.T) {
	r := terminalRenderer(false)
	renderSome(r, at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 0))
	r.live(rows("default/web", "up", 1, "\x1b[0;32m✔\x1b[0m done"))
	if a := r.area(); strings.Contains(a, "\x1b") {
		t.Errorf("area %q", a)
	}
}

func stripAll(s string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		b.WriteString(text(l, false))
	}
	return b.String()
}

// terminal is a pseudo-terminal for an app: what the app writes to it
// shows on a Screen.
type terminal struct {
	slave  *os.File
	mu     sync.Mutex
	master *os.File
	screen *ansi.Screen
}

func newTerminal(t *testing.T) *terminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 60, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	tt := &terminal{slave: slave, master: master, screen: ansi.NewScreen(60, 30)}
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			tt.mu.Lock()
			tt.screen.Write(b[:n])
			tt.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return tt
}

// shows waits until the terminal shows rows, as ansi.Strip leaves them.
func (tt *terminal) shows(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		tt.mu.Lock()
		got = nil
		for _, r := range tt.screen.Rows() {
			got = append(got, strings.TrimRight(ansi.Strip(r), " "))
		}
		tt.mu.Unlock()
		if slices.Equal(got, want) {
			return
		}
	}
	t.Fatalf("the terminal shows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
}

// The area goes before anything else goes to the terminal and comes back
// under it; while a question waits, it stays above the question, and once
// the question has its answer, under it.
func TestLiveAroundOutput(t *testing.T) {
	tt := newTerminal(t)
	in, answer := io.Pipe()
	a := &app{stdout: tt.slave, stderr: tt.slave, stdin: bufio.NewReader(in), env: map[string]string{"NO_COLOR": "1"}}
	render := a.render()
	render(at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 0))
	a.drawLive(rows("default/web", "up", 1, "one", "two"))
	tt.shows(t, "▸ web › up", "│ one", "│ two")
	render(at(logLine("default/web", "up", "", "final"), 2))
	tt.shows(t, "▸ web › up", "│ final", "│ one", "│ two")

	done := make(chan bool)
	go func() {
		ok, _ := a.approve(context.Background(), engine.Question{Op: "up", Address: "default/db", Summary: "will run: new"})
		done <- ok
	}()
	tt.shows(t, "▸ web › up", "│ final", "│ one", "│ two", "Apply db? (new) [y/N]")
	a.drawLive(rows("default/web", "up", 3, "three"))
	tt.shows(t, "▸ web › up", "│ final", "│ three", "Apply db? (new) [y/N]")
	render(at(logLine("default/web", "up", "", "more"), 4))
	tt.shows(t, "▸ web › up", "│ final", "│ more", "│ three", "Apply db? (new) [y/N]")
	// The terminal echoes the answer, then the app reads it.
	if _, err := tt.master.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	tt.shows(t, "▸ web › up", "│ final", "│ more", "│ three", "Apply db? (new) [y/N] y")
	if _, err := answer.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("y did not approve")
	}
	tt.shows(t, "▸ web › up", "│ final", "│ more", "Apply db? (new) [y/N] y", "▸ web › up", "│ three")

	// The end of the output takes the area away.
	a.endOutput()
	tt.shows(t, "▸ web › up", "│ final", "│ more", "Apply db? (new) [y/N] y")
}

// An interrupt while a question waits leaves the question above the
// notice, and the area under it.
func TestLiveInterruptedQuestion(t *testing.T) {
	tt := newTerminal(t)
	in, _ := io.Pipe()
	a := &app{stdout: tt.slave, stderr: tt.slave, stdin: bufio.NewReader(in), env: map[string]string{"NO_COLOR": "1"}}
	a.render()(at(ev("unit", "default/web", "up", "", "running", "", "up: started"), 0))
	a.drawLive(rows("default/web", "up", 1, "one"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := a.approve(ctx, engine.Question{Op: "up", Address: "default/db", Summary: "will run: new"})
		done <- err
	}()
	tt.shows(t, "▸ web › up", "│ one", "Apply db? (new) [y/N]")
	a.notice("interrupted")
	cancel()
	if err := <-done; err == nil {
		t.Fatal("approve did not end")
	}
	tt.shows(t, "Apply db? (new) [y/N]", "interrupted", "▸ web › up", "│ one")
}

// echoes reports whether the terminal f echoes what is typed.
func echoes(t *testing.T, f *os.File) bool {
	t.Helper()
	tios, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	return tios.Lflag&unix.ECHO != 0
}

// A sensitive input is typed without echo; an interrupt ends its question
// at once, as it ends any other, and the echo comes back either way.
func TestAskHidden(t *testing.T) {
	in := &model.Input{Type: "string", Sensitive: true}
	hidden := func(t *testing.T) (*terminal, *app) {
		tt := newTerminal(t)
		return tt, &app{stdout: tt.slave, stderr: tt.slave, in: tt.slave, stdin: bufio.NewReader(tt.slave), env: map[string]string{"NO_COLOR": "1"}}
	}

	tt, a := hidden(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.ask(ctx, "token", in)
		done <- err
	}()
	tt.shows(t, "token:")
	if echoes(t, tt.slave) {
		t.Fatal("the terminal echoes at the question of a sensitive input")
	}
	cancel(errors.New("interrupted by SIGINT"))
	select {
	case err := <-done:
		if err == nil || err.Error() != "interrupted by SIGINT" {
			t.Fatalf("ask ended with %v; want the interrupt", err)
		}
	case <-time.After(time.Second):
		t.Fatal("an interrupt did not end the question")
	}
	if !echoes(t, tt.slave) {
		t.Fatal("the echo stays off after an interrupt")
	}

	tt, a = hidden(t)
	got := make(chan string, 1)
	go func() {
		v, err := a.ask(context.Background(), "token", in)
		if err != nil {
			t.Error(err)
		}
		got <- v
	}()
	tt.shows(t, "token:")
	if _, err := tt.master.Write([]byte("abc\n")); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != "abc" {
		t.Fatalf("ask gave %q; want abc", v)
	}
	tt.shows(t, "token:")
	if !echoes(t, tt.slave) {
		t.Fatal("the echo stays off after the answer")
	}
}
