package daemon

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// reinstall writes yaml as the project of dir and installs its triggers,
// with PATH and environ as the environment of whoever installs them.
func reinstall(t *testing.T, d *Daemon, dir, yaml string, environ ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	d.trig.install(runOptions(dir, "default", nil, append([]string{"PATH=" + os.Getenv("PATH")}, environ...)))
}

func sourcesOf(d *Daemon) map[string]*source {
	d.trig.mu.Lock()
	defer d.trig.mu.Unlock()
	return maps.Clone(d.trig.sources)
}

func nexts(t *testing.T, d *Daemon) []string {
	t.Helper()
	var out []string
	for _, s := range d.reg.files().Schedules {
		if s.Next == "" {
			t.Fatalf("%s/%s has no next", s.Task, s.Kind)
		}
		out = append(out, s.Task+"/"+s.Kind+"/"+s.Next)
	}
	return out
}

// taken waits until the loop of the watch of s took every event pushed.
func taken(t *testing.T, s *source) {
	t.Helper()
	eventually(t, "the watch takes its events", func() bool { return len(s.w.events) == 0 })
}

// An install that changes nothing a source reads keeps every source, with
// its timer, its next time, and its program, which does not start again,
// and hands them the new snapshot.
func TestInstallKeepsSources(t *testing.T) {
	yaml := `name: shop
tasks:
  tick:
    run: "true"
    on:
      every: 1h
      schedule: {cron: "0 7 * * *", timezone: UTC}
  feed:
    run: echo "$LINE" >> lines
    env: {LINE: '${{ event.line }}'}
    on:
      watch:
        command: {argv: [sh, -c, 'echo started >> starts; exec sleep 300']}
`
	d, dir := triggerDaemon(t, yaml, nil)
	eventually(t, "the source starts", func() bool { return len(lines(t, filepath.Join(dir, "starts"))) == 1 })
	before, next := sourcesOf(d), nexts(t, d)
	if len(before) != 3 {
		t.Fatalf("sources %v", slices.Sorted(maps.Keys(before)))
	}
	feed := installed(t, d, "feed", "watch")
	svc := feed.w.svc
	reinstall(t, d, dir, yaml, "CI_JOB=1")
	reinstall(t, d, dir, yaml, "CI_JOB=2")
	after := sourcesOf(d)
	for id, s := range before {
		if after[id] != s {
			t.Fatalf("%s was not kept", id)
		}
		if s.cur.Load().snap != d.trig.snap {
			t.Fatalf("%s has an old snapshot", id)
		}
	}
	if got := nexts(t, d); !slices.Equal(got, next) {
		t.Fatalf("next %v, was %v", got, next)
	}
	if feed.w.svc != svc {
		t.Fatal("the program of the watch was started again")
	}
	// A start again prepares the program from the snapshot it has now.
	prog, err := svc.Rebuild()
	if err != nil || !prog.Equal(svc.Program) {
		t.Fatalf("rebuild %v %v", prog, err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := lines(t, filepath.Join(dir, "starts")); len(got) != 1 {
		t.Fatalf("the source started %d times", len(got))
	}
}

// Sources are matched by what they are, not by their place in the list: a
// watch moved in the list keeps its program and runs with its own with:,
// triggers that are alike stay one each, and a watch whose program changed
// starts again while the others stay.
func TestInstallMatchesBySource(t *testing.T) {
	watch := func(who, prog string) string {
		return `
        - command: {argv: [sh, -c, 'echo ` + prog + ` >> starts; exec sleep 300']}
          with: {who: ` + who + `}`
	}
	project := func(watches ...string) string {
		return `name: shop
tasks:
  feed:
    inputs:
      who: {type: string}
    run: echo "$WHO $LINE" >> fired
    env: {WHO: '${{ inputs.who }}', LINE: '${{ event.line }}'}
    on:
      every: [1h, 1h]
      watch:` + strings.Join(watches, "") + "\n"
	}
	d, dir := triggerDaemon(t, project(watch("a", "a"), watch("b", "b")), nil)
	eventually(t, "both sources start", func() bool { return len(lines(t, filepath.Join(dir, "starts"))) == 2 })
	before := sourcesOf(d)
	if len(before) != 4 {
		t.Fatalf("sources %v", slices.Sorted(maps.Keys(before)))
	}
	reinstall(t, d, dir, project(watch("b", "b"), watch("a", "a")))
	if after := sourcesOf(d); !maps.Equal(after, before) {
		t.Fatalf("sources %v, were %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	var a *source
	for _, s := range before {
		b := s.cur.Load()
		if b.tr.Kind != "watch" {
			continue
		}
		w := b.snap.opts.Model.Tasks["feed"].On.Watch[b.tr.Index]
		who := w.With["who"].(string)
		if !slices.Contains(s.w.prog.Args, "echo "+who+" >> starts; exec sleep 300") {
			t.Fatalf("the watch of %s has the program %v", who, s.w.prog.Args)
		}
		if who == "a" {
			a = s
		}
	}
	a.w.push(map[string]any{"line": "x"})
	eventually(t, "the run of a", func() bool { return slices.Contains(lines(t, filepath.Join(dir, "fired")), "a_x") })

	reinstall(t, d, dir, project(watch("b", "c"), watch("a", "a")))
	eventually(t, "the changed source starts", func() bool { return len(lines(t, filepath.Join(dir, "starts"))) == 3 })
	after := sourcesOf(d)
	kept := 0
	for id, s := range before {
		if after[id] == s {
			kept++
		}
	}
	if kept != 3 || len(after) != 4 {
		t.Fatalf("%d of %d sources kept, %d now", kept, len(before), len(after))
	}
	if got := lines(t, filepath.Join(dir, "starts")); !slices.Equal(got, []string{"a", "b", "c"}) && !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("starts %v", got)
	}
}

// A program starts again when the environment it gets changes, a variable
// that is not there differing from one that is empty, and stays when only
// variables that it does not get change.
func TestInstallEffectiveEnvironment(t *testing.T) {
	yaml := `name: shop
tasks:
  feed:
    run: "true"
    on:
      watch:
        command: {argv: [sh, -c, 'echo "home=${HOME-none}" >> starts; exec sleep 300']}
`
	d, dir := triggerDaemon(t, yaml, nil)
	starts := 1
	eventually(t, "the source starts", func() bool { return len(lines(t, filepath.Join(dir, "starts"))) == starts })
	for i, step := range []struct {
		env  []string
		kept bool
	}{
		{[]string{"FOO=1"}, true},
		{[]string{"FOO=1", "HOME="}, false},
		{[]string{"FOO=2", "HOME="}, true},
		{[]string{"HOME=/elsewhere"}, false},
	} {
		s := installed(t, d, "feed", "watch")
		reinstall(t, d, dir, yaml, step.env...)
		if kept := installed(t, d, "feed", "watch") == s; kept != step.kept {
			t.Fatalf("step %d: kept %v, want %v", i, kept, step.kept)
		}
		if !step.kept {
			starts++
			eventually(t, "the source starts again", func() bool { return len(lines(t, filepath.Join(dir, "starts"))) == starts })
		}
	}
	if got := lines(t, filepath.Join(dir, "starts")); !slices.Equal(got, []string{"home=none", "home=", "home=/elsewhere"}) {
		t.Fatalf("starts %v", got)
	}
}

// What a debounce gathered stays through an install that changes nothing,
// runs with the old snapshot when an install changes it, and runs when an
// install removes its watch, which takes no event after that.
func TestInstallBatch(t *testing.T) {
	project := func(v string, watch bool) string {
		yaml := `name: shop
tasks:
  feed:
    inputs:
      v: {type: string}
    run: echo "$V $N" >> fired
    env: {V: '${{ inputs.v }}', N: '${{ string(event.count) }}'}
`
		if watch {
			yaml += `    on:
      watch:
        command: {argv: [sh, -c, 'exec sleep 300']}
        debounce: 1h
        with: {v: ` + v + `}
`
		}
		return yaml
	}
	d, dir := triggerDaemon(t, project("one", true), nil)
	fired := filepath.Join(dir, "fired")
	s := installed(t, d, "feed", "watch")
	s.w.push(map[string]any{"line": "x"})
	taken(t, s)
	reinstall(t, d, dir, project("one", true))
	if d.trig.active() || len(lines(t, fired)) > 0 {
		t.Fatal("an install that changed nothing ran the batch")
	}
	reinstall(t, d, dir, project("two", true))
	if installed(t, d, "feed", "watch") != s {
		t.Fatal("the source was not kept")
	}
	eventually(t, "the batch runs with the old snapshot", func() bool { return slices.Equal(lines(t, fired), []string{"one_1"}) })
	s.w.push(map[string]any{"line": "y"})
	s.w.push(map[string]any{"line": "z"})
	taken(t, s)
	reinstall(t, d, dir, project("", false))
	eventually(t, "the batch of the removed watch runs", func() bool { return slices.Equal(lines(t, fired), []string{"one_1", "two_2"}) })
	if len(sourcesOf(d)) != 0 {
		t.Fatal("the removed watch is still there")
	}
	s.w.push(map[string]any{"line": "late"})
	d.trig.runs.Wait()
	if got := lines(t, fired); len(got) != 2 {
		t.Fatalf("fired %v", got)
	}
}

// A request is answered with the snapshot of the route it found, also when
// an install changes the secret, with:, and task meanwhile; the server
// stays on its address, and the next request checks the new secret.
func TestWebhookKeepsItsSnapshot(t *testing.T) {
	project := func(v string) string {
		return `name: shop
inputs:
  hook: {type: string, env: HOOK}
runtime:
  listen: 127.0.0.1:0
tasks:
  deploy:
    inputs:
      v: {type: string}
    run: echo "$V" >> fired
    env: {V: '${{ inputs.v }}'}
    on:
      webhook:
        path: /hooks/deploy
        secret: ${{ inputs.hook }}
        signature: bearer
        respond: wait
        with: {v: ` + v + `}
`
	}
	d, dir := triggerDaemon(t, project("one"), nil)
	reinstall(t, d, dir, project("one"), "HOOK=old-secret")
	listen := d.reg.files().Listen
	request := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/hooks/deploy", strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
	held := request("old-secret")
	b, _ := d.trig.hooks.route(held)
	reinstall(t, d, dir, project("two"), "HOOK=new-secret")
	rec := httptest.NewRecorder()
	d.trig.hooks.handle(rec, held, b)
	if rec.Code != http.StatusOK {
		t.Fatalf("the request held over the install got %d %s", rec.Code, rec.Body)
	}
	for token, want := range map[string]int{"old-secret": http.StatusUnauthorized, "new-secret": http.StatusOK} {
		rec := httptest.NewRecorder()
		d.trig.hooks.ServeHTTP(rec, request(token))
		if rec.Code != want {
			t.Fatalf("%s got %d, want %d", token, rec.Code, want)
		}
	}
	if got := lines(t, filepath.Join(dir, "fired")); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("fired %v", got)
	}
	if got := d.reg.files().Listen; got != listen || listen == "" {
		t.Fatalf("listen %q, was %q", got, listen)
	}
}

// A project whose watch cannot be prepared keeps the triggers installed
// before, with their snapshot.
func TestInstallKeepsWhatItCannotPrepare(t *testing.T) {
	good := `name: shop
tasks:
  feed:
    run: "true"
    on:
      watch:
        command: {argv: [sh, -c, 'exec sleep 300']}
`
	d, dir := triggerDaemon(t, good, nil)
	before, snap := sourcesOf(d), d.trig.snap
	reinstall(t, d, dir, good+`  other:
    run: "true"
    on:
      every: 1h
      watch:
        command: {argv: [sh, -c, '${{ file("missing") }}']}
`)
	if after := sourcesOf(d); !maps.Equal(after, before) || d.trig.snap != snap {
		t.Fatalf("sources %v, were %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	log, _ := os.ReadFile(LogPath(filepath.Join(dir, ".opstruss"), "default"))
	if !strings.Contains(string(log), "the triggers installed before stay: watch") {
		t.Fatalf("the log of the daemon:\n%s", log)
	}
}

// Installs that come at once make one source each, and none comes after
// the triggers closed; neither does a run.
func TestInstallsOneAtATime(t *testing.T) {
	yaml := `name: shop
tasks:
  feed:
    run: echo fired >> fired
    on:
      every: 1h
      watch:
        command: {argv: [sh, -c, 'echo started >> starts; exec sleep 300']}
`
	d, dir := triggerDaemon(t, yaml, nil)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() { reinstall(t, d, dir, yaml, "CI_JOB="+string(rune('a'+i))) })
	}
	wg.Wait()
	if n := len(sourcesOf(d)); n != 2 {
		t.Fatalf("%d sources", n)
	}
	every := installed(t, d, "feed", "every")
	d.trig.close()
	reinstall(t, d, dir, yaml, "CI_JOB=late")
	if n := len(sourcesOf(d)); n != 0 {
		t.Fatalf("%d sources after close", n)
	}
	d.trig.fire(every, time.Now())
	d.trig.runs.Wait()
	if _, err := os.Stat(filepath.Join(dir, "fired")); err == nil {
		t.Fatal("a run came after close")
	}
	if got := lines(t, filepath.Join(dir, "starts")); len(got) != 1 {
		t.Fatalf("the source started %d times", len(got))
	}
}
