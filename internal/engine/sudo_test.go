package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
)

// TestMain lets the test binary be opstruss __sudo, which sudo runs.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == proc.SudoCommand {
		os.Exit(proc.Serve(os.Stderr, os.Args[0]))
	}
	os.Exit(m.Run())
}

// fakeSudo wants the password in $HOME/.fakesudo/password until sudo -S
// -v took it, then runs its command as the same user; each call goes to
// $HOME/.fakesudo/log.
const fakeSudo = `#!/bin/sh
d=$HOME/.fakesudo
echo "$*" >> "$d/log"
n= S= v=
while [ $# -gt 0 ]; do
  case $1 in -n) n=1 ;; -S) S=1 ;; -p) shift ;; -v) v=1 ;; --) shift; break ;; *) break ;; esac
  shift
done
if [ -f "$d/password" ] && [ ! -f "$d/ticket" ]; then
  [ -n "$n" ] && { echo 'sudo: a password is required' >&2; exit 1; }
  [ -n "$S" ] || exit 1
  IFS= read -r pw
  [ "$pw" = "$(cat "$d/password")" ] || { echo 'Sorry, try again.' >&2; echo 'sudo: 1 incorrect password attempt' >&2; exit 1; }
  : > "$d/ticket"
fi
[ -n "$v" ] && exit 0
export SUDO_UID=$(id -u) SUDO_GID=$(id -g) SUDO_USER=$(id -un)
exec "$@"
`

// TestSudoPassword: sudo that wants a password asks for it once at the
// terminal, again after a wrong one, also for programs that start at the
// same time, and keeps its ticket while the run lasts; the password is in
// nothing that the run writes.
func TestSudoPassword(t *testing.T) {
	old := sudoKeep
	sudoKeep = 50 * time.Millisecond
	defer func() { sudoKeep = old }()
	home, bin, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(home, ".fakesudo")
	if err := os.MkdirAll(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	const password = "secret-password-9"
	if err := os.WriteFile(filepath.Join(fake, "password"), []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := "name: pw\nunits:\n" +
		"  a:\n    run: 'sleep 0.3; env > \"$OPSTRUSS_ARTIFACTS/env\"; echo \"a as $USER\"'\n    sudo: true\n" +
		"  b:\n    run: 'sleep 0.3; echo \"b as $USER\"'\n    sudo: true\n"
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	var events []Event
	answers := []string{"wrong", password}
	env := []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	res, err := Up(t.Context(), Options{Dir: project, Environ: env, LookupEnv: func(string) (string, bool) { return "", false }, Yes: true,
		Ask: func(_ context.Context, prompt string, in *model.Input) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if !in.Sensitive {
				t.Errorf("the password of sudo is asked for as text")
			}
			asked = append(asked, prompt)
			answer := answers[0]
			answers = answers[1:]
			return answer, nil
		},
		Emit: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * sudoKeep)
	if res.ExitCode != 0 {
		t.Fatalf("exit %d, events %v", res.ExitCode, events)
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "sudo password for ") {
		t.Fatalf("asked %q; want the password twice, the wrong one and the right one", asked)
	}
	var sorry, ran int
	for _, ev := range events {
		if strings.Contains(ev.Message, password) {
			t.Errorf("event holds the password: %+v", ev)
		}
		if ev.Message == "Sorry, try again.\nsudo: 1 incorrect password attempt" {
			sorry++
		}
		if ev.Type == "log" && (ev.Message == "a as root" || ev.Message == "b as root") {
			ran++
		}
	}
	if sorry != 1 || ran != 2 {
		t.Errorf("%d warnings of a wrong password, %d programs as root; want 1 and 2: %v", sorry, ran, events)
	}
	log, err := os.ReadFile(filepath.Join(fake, "log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), password) || !strings.Contains(string(log), "-n -v\n") {
		t.Errorf("sudo log, which has the arguments of each call and a keep-alive -n -v:\n%s", log)
	}
	// What the run keeps, the environment of the program among it, does
	// not hold the password either.
	err = filepath.WalkDir(filepath.Join(project, ".opstruss"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), password) {
			t.Errorf("%s holds the password", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Once the run ended, the keep-alive stops.
	n := strings.Count(string(log), "-n -v\n")
	time.Sleep(4 * sudoKeep)
	if log, _ = os.ReadFile(filepath.Join(fake, "log")); strings.Count(string(log), "-n -v\n") != n {
		t.Errorf("the keep-alive goes on after the run")
	}

	// With the ticket of sudo, a run asks nothing.
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(strings.ReplaceAll(yaml, "sleep 0.3", "sleep 0.1")), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = Up(t.Context(), Options{Dir: project, Environ: env, LookupEnv: func(string) (string, bool) { return "", false }, Yes: true,
		Ask: func(context.Context, string, *model.Input) (string, error) {
			t.Error("asked for the password, with a ticket of sudo")
			return "", nil
		}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exit %v, %v", res, err)
	}
}
