package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// unitCommands are the commands that change units: plan, up, and down,
// with the aliases that Terraform and Pulumi users reach for.
func (a *app) unitCommands() []*cobra.Command {
	type flags struct{ yes, force, noDeps bool }
	command := func(use, alias, short, force string) *cobra.Command {
		var f flags
		cmd := &cobra.Command{
			Use:     use + " [unit]...",
			Aliases: []string{alias},
			Short:   short,
			RunE: func(_ *cobra.Command, units []string) error {
				req, err := a.request(units)
				if err != nil {
					return err
				}
				req.Yes, req.Force, req.NoDeps = f.yes, f.force, f.noDeps
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				res, err := a.call(ctx, use, req)
				if err != nil {
					return err
				}
				if res.ExitCode != 0 {
					return &exitError{res.ExitCode}
				}
				return nil
			},
		}
		cmd.Flags().BoolVar(&f.noDeps, "no-deps", false, "leave out the units that the ones named depend on, or for down those that depend on them")
		if force != "" {
			cmd.Flags().BoolVar(&f.yes, "yes", false, "approve the changes of this run, as in CI")
			cmd.Flags().BoolVar(&f.force, "force", false, force)
		}
		return cmd
	}
	up := command("up", "apply", "Bring the units up: each runs when its definition changed",
		"run the units named although unchanged")
	up.Long = up.Short + `.

With -d, up runs in the daemon of the stack, which starts in the background
when none runs and stays until opstruss stop. While a daemon runs, the runs
of the command line go to it.`
	up.Annotations = map[string]string{detachable: "true"}
	return []*cobra.Command{
		command("plan", "preview", "Show what up would do and why, without changing anything", ""),
		up,
		command("down", "destroy", "Take the units down with the down contract of their records",
			"run down although pass values differ from those recorded"),
		command("status", "ps", "Show the state of the units: the status task of their plugin, or what their record says", ""),
	}
}

// request is a call of the engine API about the project and stack of
// this command, with the units named.
func (a *app) request(units []string) (api.Request, error) {
	dir, err := a.projectDir()
	if err != nil {
		return api.Request{}, err
	}
	given, err := a.givenInputs()
	if err != nil {
		return api.Request{}, err
	}
	debug, _ := a.lookupEnv("OPSTRUSS_LOG")
	return api.Request{Dir: dir, Cwd: a.cwd, Stack: a.stackName(), Inputs: given, Environ: a.environ, Units: units,
		Debug: debug == "debug"}, nil
}

// handlers are the callbacks of a call: events go to the renderer, and a
// terminal answers the questions of a run.
func (a *app) handlers() api.Handlers {
	h := api.Handlers{Event: a.render()}
	if a.answers() {
		h.Approve, h.Ask = a.approve, a.ask
	}
	return h
}

// answers reports whether a terminal answers the questions of a run.
func (a *app) answers() bool { return a.terminal && a.stdin != nil }

// approve asks at the terminal whether to apply one change; only y or yes
// approves it, as the prompt of terraform apply wants yes.
func (a *app) approve(ctx context.Context, q engine.Question) (bool, error) {
	who := q.Address
	if who == "" {
		who = q.Task
	}
	fmt.Fprintf(a.stderr, "%s: %s\nApply %s? [y/N] ", who, q.Summary, who)
	line, err := a.readLine(ctx)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// ask asks at the terminal for a required input that has no value.
func (a *app) ask(ctx context.Context, name string, in *model.Input) (string, error) {
	prompt := name
	if in.Description != "" {
		prompt += " (" + in.Description + ")"
	}
	fmt.Fprintf(a.stderr, "%s: ", prompt)
	if f, ok := a.in.(*os.File); ok && in.Sensitive && term.IsTerminal(int(f.Fd())) {
		// A sensitive input is typed without echo.
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		return string(b), err
	}
	line, err := a.readLine(ctx)
	return strings.TrimRight(line, "\r\n"), err
}

// readLine reads one line of stdin, or gives up when ctx ends.
func (a *app) readLine(ctx context.Context) (string, error) {
	got := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		line, err := a.stdin.ReadString('\n')
		if err != nil && line == "" {
			errs <- err
			return
		}
		got <- line
	}()
	select {
	case line := <-got:
		return line, nil
	case err := <-errs:
		return "", err
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

// execCommand is opstruss exec <unit> -- <program>: the program runs with
// what the programs of the unit get, on this terminal, while the events of
// the run go to stderr, so its output stays its own.
func (a *app) execCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <unit> -- <program> [arg]...",
		Short: "Run a program with what the programs of one unit get, under its lock, on this terminal",
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return errors.New("write opstruss exec <unit> -- <program>, with the arguments of the program after it")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			req, err := a.request(args[:1])
			if err != nil {
				return err
			}
			e := api.New(api.Handlers{Event: a.renderTo(a.stderr)})
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := e.Exec(ctx, req, args[1:], engine.Stdio{In: a.in, Out: a.stdout, Err: a.stderr})
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return &exitError{res.ExitCode}
			}
			return nil
		},
	}
}

// render prints events: with --json one per line, otherwise a line per
// event led by its address, and program output after "| ".
func (a *app) render() func(engine.Event) { return a.renderTo(a.stdout) }

// renderTo is render with the events that are not warnings or errors on
// out.
func (a *app) renderTo(out io.Writer) func(engine.Event) {
	var mu sync.Mutex
	return func(ev engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		if a.json {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(out, "%s\n", data)
			return
		}
		w := out
		if ev.Level != "info" {
			w = a.stderr
		}
		who := ev.Address
		if who == "" && ev.Task != "" {
			who = ev.Task
			if ev.Step != "" {
				who += ": " + ev.Step
			}
		}
		switch {
		case ev.Type == "log":
			fmt.Fprintf(w, "%s | %s\n", who, ev.Message)
		case ev.Type == "run":
			for _, k := range slices.Sorted(maps.Keys(ev.Outputs)) {
				fmt.Fprintf(w, "  %s: %s\n", k, expr.Text(ev.Outputs[k]))
			}
		case who != "":
			fmt.Fprintf(w, "%s: %s\n", who, ev.Message)
		default:
			fmt.Fprintln(w, ev.Message)
		}
	}
}
