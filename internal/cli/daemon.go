package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/daemon"
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
// process otherwise; with -d, a daemon is started first when none runs.
func (a *app) call(ctx context.Context, method string, req api.Request) (*api.Result, error) {
	c, err := a.dial(req)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return a.api().Call(ctx, method, req)
	}
	return c.Call(ctx, method, req, a.handlers())
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
				d, err = daemon.Start(daemon.Options{Dir: dir, Stack: a.stack, Version: version()})
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
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			_, _ = fmt.Fprintln(ready, "ready")
			_ = ready.Close()
			return d.Serve(ctx)
		},
	}
}

// stopCommand stops the daemon of the stack, which cancels the runs in it.
func (a *app) stopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon of the stack, which cancels the runs in it",
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
			pid, err := daemon.Stop(filepath.Join(dir, ".opstruss"), stack)
			if err != nil {
				return err
			}
			if a.json {
				out := map[string]any{"stack": stack, "stopped": pid != 0}
				if pid != 0 {
					out["pid"] = pid
				}
				return a.printJSON(out)
			}
			if pid == 0 {
				fmt.Fprintf(a.stdout, "no daemon runs for stack %s\n", stack)
			} else {
				fmt.Fprintf(a.stdout, "stopped the daemon of stack %s, pid %d\n", stack, pid)
			}
			return nil
		},
	}
}
