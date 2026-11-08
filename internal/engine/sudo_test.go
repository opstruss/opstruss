package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
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
// $HOME/.fakesudo/log. With $HOME/.fakesudo/denied, it refuses the user
// after any password. It says what sudo 1.9 says.
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
  [ -f "$d/denied" ] && { echo "Sorry, user $(id -un) may not run sudo on $(uname -n)." >&2; exit 1; }
  [ "$pw" = "$(cat "$d/password")" ] || { printf 'Sorry, try again.\n\nsudo: no password was provided\nsudo: 1 incorrect password attempt\n' >&2; exit 1; }
  : > "$d/ticket"
fi
[ -n "$v" ] && exit 0
export SUDO_UID=$(id -u) SUDO_GID=$(id -g) SUDO_USER=$(id -un)
exec "$@"
`

// sudoPass is the password that the fake sudo of sudoProject wants.
const sudoPass = "secret-password-9"

// sudoProject writes a project with yaml and a fake sudo that wants
// sudoPass, and returns the project, the environment of a run, and the
// directory of the fake.
func sudoProject(t *testing.T, yaml string) (project string, env []string, fake string) {
	t.Helper()
	home, bin, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o700); err != nil {
		t.Fatal(err)
	}
	fake = filepath.Join(home, ".fakesudo")
	if err := os.MkdirAll(fake, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "password"), []byte(sudoPass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return project, []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}, fake
}

// TestSudoPassword: sudo that wants a password asks for it once at the
// terminal, again after a wrong one, also for programs that start at the
// same time, and keeps its ticket while the run lasts; the password is in
// nothing that the run writes.
func TestSudoPassword(t *testing.T) {
	old := sudoKeep
	sudoKeep = 50 * time.Millisecond
	defer func() { sudoKeep = old }()
	yaml := "name: pw\nunits:\n" +
		"  a:\n    run: 'sleep 0.3; env > \"$OPSTRUSS_ARTIFACTS/env\"; echo \"a as $USER\"'\n    sudo: true\n" +
		"  b:\n    run: 'sleep 0.3; echo \"b as $USER\"'\n    sudo: true\n"
	project, env, fake := sudoProject(t, yaml)
	var mu sync.Mutex
	var asked []string
	var events []Event
	answers := []string{"wrong", sudoPass}
	const password = sudoPass
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
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "sudo password for ") || !strings.HasSuffix(asked[0], " (a)") && !strings.HasSuffix(asked[0], " (b)") {
		t.Fatalf("asked %q; want the password twice, the wrong one and the right one, for a unit that it names", asked)
	}
	var sorry, ran int
	for _, ev := range events {
		if strings.Contains(ev.Message, password) {
			t.Errorf("event holds the password: %+v", ev)
		}
		if strings.Contains(ev.Message, "incorrect password attempt") || strings.Contains(ev.Message, "no password was provided") {
			t.Errorf("event holds what sudo said of a wrong password: %+v", ev)
		}
		if ev.Message == "sudo did not take the password; try again" && ev.Level == "warn" {
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

// An interrupt at the password prompt of sudo ends the run as an
// interrupt does, without the words of a password that sudo did not take,
// and a program that waits for the same prompt, or still starts, asks
// nothing.
func TestSudoPasswordInterrupted(t *testing.T) {
	yaml := "name: pw\nunits:\n" +
		"  a:\n    run: 'echo a'\n    sudo: true\n" +
		"  b:\n    run: 'echo b'\n    sudo: true\n"
	project, env, _ := sudoProject(t, yaml)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	var mu sync.Mutex
	var events []Event
	asked := 0
	res, err := Up(ctx, Options{Dir: project, Environ: env, LookupEnv: func(string) (string, bool) { return "", false }, Yes: true,
		Ask: func(ctx context.Context, _ string, _ *model.Input) (string, error) {
			mu.Lock()
			asked++
			mu.Unlock()
			// As the terminal does at Ctrl-C.
			cancel(Interrupted{Signal: syscall.SIGINT})
			return "", context.Cause(ctx)
		},
		Emit: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked %d times; want once", asked)
	}
	var prompt int
	for _, ev := range events {
		if ev.Type != "unit" || ev.Status != "finished" {
			continue
		}
		if ev.Outcome != cancelled {
			t.Errorf("%s ended %s: %s; want cancelled", ev.Address, ev.Outcome, ev.Message)
		}
		for _, bad := range []string{"did not take", "password of sudo:"} {
			if strings.Contains(ev.Message, bad) {
				t.Errorf("%s: %q holds %q", ev.Address, ev.Message, bad)
			}
		}
		if ev.Message == "interrupted: at the password prompt of sudo" {
			prompt++
		} else if !strings.HasPrefix(ev.Message, "interrupted: ") {
			t.Errorf("%s ended with %q; want an interrupt", ev.Address, ev.Message)
		}
	}
	if prompt != 1 || res.ExitCode == 0 {
		t.Errorf("exit %d, %d units interrupted at the prompt; want 1: %v", res.ExitCode, prompt, events)
	}
}

// sudoRun runs up in a project of two units that run as root, with a
// fake sudo that wants a password, answers each question with answer,
// and returns how often it asked and how each unit ended.
func sudoRun(t *testing.T, denied bool, answer string) (asked int, ended map[string]Event) {
	t.Helper()
	yaml := "name: pw\nunits:\n" +
		"  a:\n    run: 'echo a'\n    sudo: true\n" +
		"  b:\n    run: 'echo b'\n    sudo: true\n"
	project, env, fake := sudoProject(t, yaml)
	if denied {
		if err := os.WriteFile(filepath.Join(fake, "denied"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	ended = map[string]Event{}
	_, err := Up(t.Context(), Options{Dir: project, Environ: env, LookupEnv: func(string) (string, bool) { return "", false }, Yes: true,
		Ask: func(context.Context, string, *model.Input) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			asked++
			return answer, nil
		},
		Emit: func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			if ev.Type == "unit" && ev.Status == "finished" {
				ended[ev.Address] = ev
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	return asked, ended
}

// Three wrong passwords are all that a run asks for, whatever programs
// want sudo.
func TestSudoPasswordTries(t *testing.T) {
	asked, ended := sudoRun(t, false, "wrong")
	if asked != 3 {
		t.Errorf("asked %d times; want 3 for the run", asked)
	}
	msgs := []string{ended["default/a"].Message, ended["default/b"].Message}
	if len(ended) != 2 || !slices.Contains(msgs, "sudo did not take the password after 3 attempts") ||
		!slices.Contains(msgs, "sudo did not take the password earlier in this run") {
		t.Errorf("units ended %v", ended)
	}
}

// sudo that refuses the user, not the password, ends the questions.
func TestSudoPasswordRefused(t *testing.T) {
	asked, ended := sudoRun(t, true, sudoPass)
	if asked != 1 {
		t.Errorf("asked %d times; want once", asked)
	}
	var refused, earlier int
	for _, ev := range ended {
		switch {
		case strings.HasPrefix(ev.Message, "sudo refused: Sorry, user ") && strings.Contains(ev.Message, " may not run sudo on "):
			refused++
		case strings.HasPrefix(ev.Message, "sudo refused earlier in this run: Sorry, user "):
			earlier++
		}
	}
	if refused != 1 || earlier != 1 {
		t.Errorf("units ended %v", ended)
	}
}
