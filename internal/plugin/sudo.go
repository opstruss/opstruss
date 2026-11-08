package plugin

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
)

// root is the user root, as the passwd of this machine has it.
var root = sync.OnceValues(func() (*user.User, error) { return user.LookupId("0") })

// rootUser sets HOME and USER of env to those of root, as sudo -H does,
// unless own, the env: of the call, writes them.
func rootUser(env map[string]string, own map[string]model.EnvValue) {
	u, err := root()
	if err != nil {
		return
	}
	for k, v := range map[string]string{"HOME": u.HomeDir, "USER": u.Username} {
		if _, set := own[k]; !set {
			env[k] = v
		}
	}
}

// sudo is how the programs of the run start as root: sudo from the PATH of
// the operator, with the clean environment, and this binary as __sudo.
func (r *Run) sudo() (proc.Sudo, error) {
	path, err := LookPath("sudo", r.operator["PATH"])
	if err != nil {
		return proc.Sudo{}, errors.New("sudo is not in PATH, and a program with sudo: true runs through it")
	}
	exe, err := os.Executable()
	if err != nil {
		return proc.Sudo{}, err
	}
	var env []string
	for _, k := range slices.Sorted(maps.Keys(r.base)) {
		env = append(env, k+"="+r.base[k])
	}
	return proc.Sudo{Path: path, Exe: exe, Env: env}, nil
}

// SudoCommand is the command that a program of the run with sudo: true
// runs through, as a rule of sudoers names it without a password.
func (r *Run) SudoCommand() (string, error) {
	s, err := r.sudo()
	if err != nil {
		return "", err
	}
	return s.Exe + " " + proc.SudoCommand, nil
}

// startAsRoot starts the program of cmd as root through opstruss __sudo,
// with the descriptors that cmd gives it, and the directories of the run
// whose files it makes go back to the operator.
func (r *Run) startAsRoot(ctx context.Context, c *Call, cmd *exec.Cmd, stop Stop) (*proc.SudoProgram, error) {
	s, err := r.sudo()
	if err != nil {
		return nil, err
	}
	stdin, _ := cmd.Stdin.(*os.File)
	if stdin == nil {
		if stdin, err = os.Open(os.DevNull); err != nil {
			return nil, err
		}
		defer func() { _ = stdin.Close() }()
	}
	files := append([]*os.File{stdin, cmd.Stdout.(*os.File), cmd.Stderr.(*os.File)}, cmd.ExtraFiles...)
	own := []string{r.Temp, r.Artifacts}
	if u := c.Unit; u != nil {
		for _, d := range []string{u.Workdir, u.StateDir} {
			if d != "" {
				own = append(own, d)
			}
		}
	}
	return s.Start(ctx, proc.Spec{Path: cmd.Path, Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Env, Signal: proc.SignalName(stop.Signal),
		Grace: stop.Grace, Own: own}, files)
}

// SudoValidate gives sudo the password of the operator once, as sudo -S -v
// reads it on its stdin, so the programs of the run start through sudo -n
// while sudo keeps the ticket it makes; nothing else gets the password. An
// error of type *proc.SudoRefused holds what sudo said when it refused.
func (r *Run) SudoValidate(ctx context.Context, password string) error {
	s, err := r.sudo()
	if err != nil {
		return err
	}
	var said bytes.Buffer
	cmd := exec.CommandContext(ctx, s.Path, "-S", "-p", "", "-v")
	// In English, so what sudo says can be told apart; sudo -v runs no
	// program that would get it.
	env := append(slices.Clone(s.Env), "LC_ALL=C")
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, strings.NewReader(password+"\n"), &said, &said
	// Ctrl-C at the terminal reaches OpsTruss, which ends the run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return sudoSaid(said.String())
	}
	return nil
}

// sudoSaid is the refusal of sudo -S -v, which said out: a wrong password,
// or the lines that say why else, without the end of stdin that sudo finds
// when it reads it again for the next try.
func sudoSaid(out string) *proc.SudoRefused {
	if strings.Contains(out, "incorrect password attempt") || strings.Contains(out, "Sorry, try again") {
		return &proc.SudoRefused{Message: "the password is wrong", WrongPassword: true}
	}
	var lines []string
	for l := range strings.Lines(out) {
		if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, "no password was provided") {
			lines = append(lines, l)
		}
	}
	return &proc.SudoRefused{Message: strings.Join(lines, "; ")}
}

// SudoRefresh keeps the ticket of sudo for the run, as sudo -n -v does; a
// rule of sudoers that allows only __sudo has no ticket to keep, so the
// caller ignores the error.
func (r *Run) SudoRefresh(ctx context.Context) error {
	s, err := r.sudo()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.Path, "-n", "-v")
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Run()
}

// SudoProbe says whether sudo runs opstruss __sudo without a password, as
// a program of the run would start: nil, or a *proc.SudoRefused with what
// sudo said. It runs no program.
func (r *Run) SudoProbe() error {
	s, err := r.sudo()
	if err != nil {
		return err
	}
	return s.Probe()
}
