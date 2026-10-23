package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/daemon"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/spf13/cobra"
)

// detachable marks the commands that take -d.
const detachable = "detachable"

// daemonName is the hidden command of a daemon that -d starts.
const daemonName = "__daemon"

// api is the engine API of this process.
func (a *app) api() *api.API { return api.New(a.handlers()) }

// call runs a method of the engine API in the daemon of the stack when one
// runs, so the run shares its locks and its concurrency, and in this
// process otherwise; with -d, a daemon is started first when none runs. A
// run in this process that starts a service makes this process the daemon
// of the stack, which stays in the foreground until its services end or
// ctx does, and then stops them.
func (a *app) call(ctx context.Context, method string, req api.Request) (*api.Result, error) {
	c, err := a.dial(req)
	if err != nil {
		return nil, err
	}
	if c != nil {
		return c.Call(ctx, method, req, a.handlers())
	}
	fg := a.foreground(ctx, req)
	x := a.api()
	x.Supervisor = fg
	res, err := x.Call(ctx, method, req)
	if fg.Started() {
		a.render()(engine.Event{Type: "daemon", Level: "info",
			Message: fmt.Sprintf("this process is the daemon of stack %s while its services run; Ctrl-C stops them", fg.Stack)})
		fg.Wait(ctx)
	}
	return res, err
}

// foreground is the supervisor of a run in this process, which shows the
// output of the services it starts at this terminal.
func (a *app) foreground(ctx context.Context, req api.Request) *daemon.Foreground {
	stack, _ := model.StackOf(req.Dir, req.Stack)
	return &daemon.Foreground{Dir: req.Dir, Stack: stack, Version: version(), Ctx: ctx, Watch: a.render(),
		Environ: req.Environ, Inputs: req.Inputs}
}

// dial connects to the daemon of the request's stack, the one that runs or
// one that -d starts, or returns nil for a call in this process.
func (a *app) dial(req api.Request) (*api.Client, error) {
	root := filepath.Join(req.Dir, ".opstruss")
	if _, err := os.Stat(filepath.Join(root, "daemon")); err != nil && !a.detach {
		return nil, nil
	}
	stack, err := model.StackOf(req.Dir, req.Stack)
	if err != nil && !a.detach {
		// The run in this process says what is wrong.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := daemon.Dial(root, stack, version(), a.answers())
	if err == nil && c == nil && a.detach {
		if err = a.startDaemon(req.Dir, stack); err == nil {
			c, err = daemon.Dial(root, stack, version(), a.answers())
		}
	}
	var pe *api.ProtocolError
	if errors.As(err, &pe) {
		return nil, fmt.Errorf("the daemon of stack %s runs opstruss %s, which speaks protocol %d, not %d: stop it with opstruss stop, then start it again with opstruss up -d",
			stack, pe.Version, pe.Protocol, api.Protocol)
	}
	return c, err
}

// startDaemon starts the daemon of stack in the background: this program
// again, in a session of its own, which writes on its descriptor 3 that it
// listens, or why it did not start. What it writes on stderr, such as a
// crash, goes to _daemon.stderr beside its log.
func (a *app) startDaemon(dir, stack string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logs := filepath.Dir(daemon.LogPath(filepath.Join(dir, ".opstruss"), stack))
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return err
	}
	stderr, err := os.OpenFile(filepath.Join(logs, "_daemon.stderr"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = stderr.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(exe, "-C", dir, daemonName, "--stack", stack)
	// The daemon is no program of a run that starts it.
	cmd.Env = slices.DeleteFunc(slices.Clone(a.environ), func(kv string) bool {
		return strings.HasPrefix(kv, "OPSTRUSS_RUN=") || strings.HasPrefix(kv, "OPSTRUSS_RUN_PARENTS=")
	})
	cmd.Dir, cmd.Stderr, cmd.ExtraFiles = dir, stderr, []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		return err
	}
	_ = cmd.Process.Release()
	said, _ := io.ReadAll(r)
	switch msg := strings.TrimSpace(string(said)); msg {
	case "ready", "running":
		return nil
	case "":
		return fmt.Errorf("the daemon of stack %s stopped as it started; see %s", stack, stderr.Name())
	default:
		return errors.New(msg)
	}
}

// daemonCommand is the daemon that -d starts: it says on descriptor 3 that
// it listens, or that another daemon of the stack runs already, or why it
// did not start, and serves until SIGTERM, which opstruss stop sends.
func (a *app) daemonCommand() *cobra.Command {
	return &cobra.Command{
		Use:    daemonName,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			ready := os.NewFile(3, "ready")
			defer func() { _ = ready.Close() }()
			dir, err := a.projectDir()
			var d *daemon.Daemon
			if err == nil {
				d, err = daemon.Start(daemon.Options{Dir: dir, Stack: a.stack, Version: version(), Environ: a.environ})
			}
			var running *daemon.RunningError
			if errors.As(err, &running) {
				_, _ = fmt.Fprintln(ready, "running")
				return nil
			}
			if err != nil {
				_, _ = fmt.Fprintln(ready, err)
				return err
			}
			// A daemon that a signal stops ends with 0, as a service should.
			in := a.interrupts()
			defer in.stop()
			_, _ = fmt.Fprintln(ready, "ready")
			_ = ready.Close()
			return d.Serve(in.ctx)
		},
	}
}

// stopCommand stops the daemon of the stack, which cancels the runs in it
// and stops its services; their records stay, so up starts them again.
// What a daemon that ended left of its services is stopped too.
func (a *app) stopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon of the stack, which cancels the runs in it and stops its services",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := a.projectDir()
			if err != nil {
				return err
			}
			stack, err := model.StackOf(dir, a.stackName())
			if err != nil {
				return err
			}
			root := filepath.Join(dir, ".opstruss")
			pid, err := daemon.Stop(root, stack)
			if err != nil {
				return err
			}
			left, unclear, err := daemon.Recover(root, stack, "")
			var running *daemon.RunningError
			if err != nil && !errors.As(err, &running) {
				return err
			}
			if a.json {
				out := map[string]any{"stack": stack, "stopped": pid != 0}
				if pid != 0 {
					out["pid"] = pid
				}
				if len(left) > 0 {
					out["left"] = left
				}
				if err := a.printJSON(out); err != nil {
					return err
				}
			} else {
				if pid == 0 {
					fmt.Fprintf(a.stdout, "no daemon runs for stack %s\n", stack)
				} else {
					fmt.Fprintf(a.stdout, "stopped the daemon of stack %s, pid %d\n", stack, pid)
				}
				for _, l := range left {
					fmt.Fprintf(a.stdout, "stopped what the daemon that ended left: %s\n", l)
				}
			}
			if len(unclear) > 0 {
				return fmt.Errorf("%s; once you are sure it is not the service's, opstruss unlock <unit> forgets it", strings.Join(unclear, "; "))
			}
			return nil
		},
	}
}

// unlockCommand forgets what is left of a service that cannot be proven
// its own, which keeps up from starting it and stop from finishing.
func (a *app) unlockCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <unit>",
		Short: "Forget what is left of a service that cannot be proven its own, so up starts it again",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, units []string) error {
			req, err := a.request(units)
			if err != nil {
				return err
			}
			in := a.interrupts()
			defer in.stop()
			res, err := a.call(in.ctx, "unlock", req)
			if err != nil {
				return err
			}
			return in.exit(res.ExitCode)
		},
	}
}
