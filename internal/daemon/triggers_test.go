package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
)

// triggerDaemon starts the daemon of a project whose files are yaml, in a
// short directory for its socket.
func triggerDaemon(t *testing.T, yaml string, before func(root string)) (*Daemon, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	if before != nil {
		before(filepath.Join(dir, ".opstruss"))
	}
	d, err := Start(Options{Dir: dir, Stack: "default", Version: "v1", Environ: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = d.Serve(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-served
	})
	return d, dir
}

func lines(t *testing.T, p string) []string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return strings.Fields(strings.ReplaceAll(string(data), " ", "_"))
}

// if: of a schedule leaves a time out without a run, with: gives the
// inputs of the run, and a time that comes while the run it made last
// still runs is taken by that run.
func TestScheduleFire(t *testing.T) {
	d, dir := triggerDaemon(t, `name: shop
tasks:
  report:
    inputs:
      day: {type: string, required: true}
    run: sleep "$WAIT"; echo "$DAY-$TRIGGER" >> fired
    env: {DAY: '${{ inputs.day }}', TRIGGER: '${{ event.trigger }}', WAIT: '${{ inputs.day == "1" ? "1" : "0" }}'}
    on:
      schedule:
        cron: "0 7 * * *"
        timezone: UTC
        if: event.time.getDayOfWeek() != 0
        with:
          day: ${{ string(event.time.getDayOfWeek()) }}
`, nil)
	list, _, err := engine.Triggers(runOptions(dir, "default", nil, []string{"PATH=" + os.Getenv("PATH")}))
	if err != nil || len(list) != 1 {
		t.Fatalf("triggers %v %v", list, err)
	}
	tr := list[0]
	monday := time.Date(2026, 10, 26, 7, 0, 0, 0, time.UTC)
	d.trig.fire(tr, monday)
	d.trig.fire(tr, monday.Add(time.Second)) // the run of monday still sleeps
	d.trig.fire(tr, time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC))
	d.trig.runs.Wait()
	d.trig.fire(tr, time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC)) // Sunday: if: leaves it out
	d.trig.runs.Wait()
	if got := strings.Join(lines(t, filepath.Join(dir, "fired")), " "); got != "1-schedule" {
		t.Fatalf("fired %q", got)
	}
	log, _ := os.ReadFile(LogPath(filepath.Join(dir, ".opstruss"), "default"))
	for _, want := range []string{"the run it made last runs or waits still, and takes the time of 2026-10-26T07:00:01Z too",
		"if: left the time of 2026-10-25T07:00:00Z out"} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("the log of the daemon has no %q:\n%s", want, log)
		}
	}
}

// A persistent schedule whose last run is older than its last time runs
// once as the daemon starts, at the last time it missed.
func TestPersistentCatchUp(t *testing.T) {
	last := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	yaml := `name: shop
tasks:
  backup:
    run: echo "$TIME" >> fired
    env: {TIME: '${{ string(event.time) }}'}
    on:
      schedule: {cron: "* * * * *", timezone: UTC, persistent: true}
`
	_, dir := triggerDaemon(t, yaml, func(root string) {
		list, _, err := engine.Triggers(runOptions(filepath.Dir(root), "default", nil, nil))
		if err != nil || len(list) != 1 {
			t.Fatalf("triggers %v %v", list, err)
		}
		if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o700); err != nil {
			t.Fatal(err)
		}
		f := File{Version: Version, Schedules: []*Schedule{{Task: "backup", Kind: "schedule", Key: list[0].Key, Last: last.Format(time.RFC3339)}}}
		if err := writeFile(filePath(root, "default"), f); err != nil {
			t.Fatal(err)
		}
	})
	want := time.Now().UTC().Truncate(time.Minute)
	eventually(t, "the run that catches up", func() bool { return len(lines(t, filepath.Join(dir, "fired"))) > 0 })
	got := lines(t, filepath.Join(dir, "fired"))
	if at, err := time.Parse(time.RFC3339, got[0]); err != nil || (!at.Equal(want) && !at.Equal(want.Add(-time.Minute))) {
		t.Fatalf("caught up at %v, want %s", got, want)
	}
}
