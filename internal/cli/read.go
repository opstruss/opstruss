package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/daemon"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/spf13/cobra"
)

func (a *app) listCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the tasks of the project with their last run, and the units with their state",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			x := a.api()
			x.Supervisor = a.foreground(context.Background(), req)
			st, err := x.Status(context.Background(), req)
			if err != nil {
				return err
			}
			// When a task comes next is what its daemon knows.
			next := daemon.Next(filepath.Join(req.Dir, ".opstruss"), st.Stack)
			for _, t := range st.Tasks {
				if at, ok := next[t.Name]; ok {
					t.Next = at.Format(time.RFC3339)
				}
			}
			if a.json {
				return a.printJSON(st)
			}
			// Columns are as wide as what they hold, so a long description
			// does not run into the last run.
			tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			if len(st.Tasks) > 0 {
				fmt.Fprintln(tw, "Tasks")
				for _, t := range st.Tasks {
					last := "never run"
					if t.Run != "" {
						last = t.Outcome + " in run " + t.Run
					}
					if t.Via != "" {
						last += " via " + t.Via
					}
					desc := t.Description
					if t.Internal {
						desc = "(internal) " + desc
					}
					if len(t.Triggers) > 0 {
						last += "; on " + strings.Join(t.Triggers, ", ")
					}
					if t.Next != "" {
						last += "; next " + t.Next
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.Name, desc, last)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if len(st.Units) > 0 {
				fmt.Fprintln(tw, "Units")
				for _, u := range st.Units {
					fmt.Fprintf(tw, "  %s\t%s\t%s\n", u.Address, u.Kind, u.State)
				}
			}
			return tw.Flush()
		},
	}
}

func (a *app) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine has what a run of the stack needs, without running it",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			rep, err := a.api().Doctor(context.Background(), req)
			if err != nil {
				return err
			}
			if a.json {
				err = a.printJSON(rep)
			} else {
				for _, c := range rep.Checks {
					fmt.Fprintf(a.stdout, "%-5s  %s\n", c.Status, c.Message)
				}
			}
			if err == nil && rep.Failed() {
				return &exitError{1}
			}
			return err
		},
	}
}

func (a *app) outputsCommand() *cobra.Command {
	var show bool
	cmd := &cobra.Command{
		Use:   "outputs",
		Short: "Print the outputs of the project from the stored outputs of its units",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			req.ShowSensitive = show
			out, err := a.api().Outputs(context.Background(), req)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(out)
			}
			if len(out) == 0 {
				// Every output the project declares is here, with a value or
				// as an error, so none is not an output still to come.
				fmt.Fprintln(a.stderr, "the project declares no outputs")
				return nil
			}
			for _, k := range slices.Sorted(maps.Keys(out)) {
				fmt.Fprintf(a.stdout, "%s: %s\n", k, expr.Text(out[k]))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&show, "show-sensitive", false, "print sensitive outputs as they are")
	return cmd
}

func (a *app) runsCommand() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "runs [run]",
		Short: "List the runs of the stack, or show the events of one as it ran",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			follow = follow || a.follow
			if follow && len(args) == 0 {
				return errors.New("-f follows one run: write opstruss runs <run> -f")
			}
			if len(args) == 1 {
				// The events of the run, rendered as when it ran, or as
				// they are stored with --json; with -f until it ends, or
				// Ctrl-C, which ends this reader with 0.
				req.Run, req.Follow = args[0], follow
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return api.New(api.Handlers{Event: a.render()}).Events(ctx, req)
			}
			runs, err := a.api().Runs(context.Background(), req)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(runs)
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			for _, r := range runs {
				what := r.Command
				if r.Task != "" {
					what = r.Task
				}
				started := r.Started
				if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
					started = t.Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Run, what, r.Outcome, started)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "go on with what a run that still runs writes, until it ends; -f is the same")
	cmd.AddCommand(a.cancelCommand(), a.rerunCommand())
	return cmd
}

// rerunCommand runs a run again, as a run of its own command: in the
// daemon of the stack when one runs, and with this caller's approval.
func (a *app) rerunCommand() *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "rerun <run>",
		Short: "Run a task, up, or destroy again with the inputs and the event of a run, through the project as it is now; approval is asked again",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			if len(req.Inputs) > 0 {
				return errors.New("runs rerun gives the run the inputs it had; --input is for a new run of a task, up, or destroy")
			}
			req.Run, req.Yes, req.Force = args[0], yes, force
			in := a.interrupts()
			defer in.stop()
			res, err := a.call(in.ctx, "rerun", req)
			if err != nil {
				return err
			}
			return in.exit(res.ExitCode)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve the changes of this run, as in CI")
	cmd.Flags().BoolVar(&force, "force", false, "for up, run the units named although unchanged; for destroy, although pass values differ")
	return cmd
}

// cancelCommand asks a run to cancel, wherever it runs, and waits until it
// ends: 0 when it ends cancelled, 1 when it ended otherwise. Ctrl-C stops
// the wait; the request stays.
func (a *app) cancelCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "cancel <run>",
		Short: "Cancel a run of the stack, as Ctrl-C at its terminal does, and wait until it ends",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			req.Run, req.Force = args[0], force
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			rec, err := a.api().Cancel(ctx, req)
			switch {
			case errors.Is(err, record.ErrEnded):
				if a.json {
					_ = a.printJSON(rec)
				}
				return fmt.Errorf("run %s already ended %s", rec.Run, rec.Outcome)
			case err != nil && ctx.Err() != nil:
				a.notice(fmt.Sprintf("stopped waiting; run %s has the request to cancel", args[0]))
				return &exitError{130}
			case err != nil:
				return err
			}
			if a.json {
				if err := a.printJSON(rec); err != nil {
					return err
				}
			}
			said := ""
			switch {
			case rec.Status == record.Running:
				// A run that only read keeps no record once it ends.
				said = "ended"
			case rec.Outcome == "cancelled":
				said = "cancelled"
			default:
				return fmt.Errorf("run %s ended %s before the request to cancel reached it", rec.Run, rec.Outcome)
			}
			if !a.json {
				fmt.Fprintf(a.stdout, "run %s %s\n", rec.Run, said)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "kill the programs of the run at once, without the rest of their stop.grace, as a second Ctrl-C does")
	return cmd
}
