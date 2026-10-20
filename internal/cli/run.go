package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/spf13/cobra"
)

// unitCommands are the commands that change units: plan, up, and down,
// with the aliases that Terraform and Pulumi users reach for.
func (a *app) unitCommands() []*cobra.Command {
	type flags struct{ yes, force, noDeps bool }
	command := func(use, alias, short, force string, fn func(context.Context, engine.Options) (*engine.Result, error)) *cobra.Command {
		var f flags
		cmd := &cobra.Command{
			Use:     use + " [unit]...",
			Aliases: []string{alias},
			Short:   short,
			RunE: func(_ *cobra.Command, units []string) error {
				opts, err := a.engineOptions(units)
				if err != nil {
					return err
				}
				opts.Yes, opts.Force, opts.NoDeps = f.yes, f.force, f.noDeps
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				res, err := fn(ctx, opts)
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
	return []*cobra.Command{
		command("plan", "preview", "Show what up would do and why, without changing anything", "",
			func(_ context.Context, o engine.Options) (*engine.Result, error) { return engine.Plan(o) }),
		command("up", "apply", "Bring the units up: each runs when its definition changed",
			"run the units named although unchanged", engine.Up),
		command("down", "destroy", "Take the units down with the down contract of their records",
			"run down although pass values differ from those recorded", engine.Down),
	}
}

func (a *app) engineOptions(units []string) (engine.Options, error) {
	dir, err := a.projectDir()
	if err != nil {
		return engine.Options{}, err
	}
	given, err := a.givenInputs()
	if err != nil {
		return engine.Options{}, err
	}
	return engine.Options{Dir: dir, Stack: a.stackName(), Inputs: given, LookupEnv: a.lookupEnv, Environ: a.environ,
		Units: units, Emit: a.render()}, nil
}

// render prints events: with --json one per line, otherwise a line per
// event led by its address, and program output after "| ".
func (a *app) render() func(engine.Event) {
	var mu sync.Mutex
	return func(ev engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		if a.json {
			data, _ := json.Marshal(ev)
			fmt.Fprintf(a.stdout, "%s\n", data)
			return
		}
		w := a.stdout
		if ev.Level == "error" || ev.Level == "warn" {
			w = a.stderr
		}
		switch {
		case ev.Type == "log":
			fmt.Fprintf(w, "%s | %s\n", ev.Address, ev.Message)
		case ev.Address != "":
			fmt.Fprintf(w, "%s: %s\n", ev.Address, ev.Message)
		default:
			fmt.Fprintln(w, ev.Message)
		}
	}
}
