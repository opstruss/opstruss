package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/expr"
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
			if a.json {
				return a.printJSON(st)
			}
			if len(st.Tasks) > 0 {
				fmt.Fprintln(a.stdout, "Tasks")
				for _, t := range st.Tasks {
					last := "never run"
					if t.Run != "" {
						last = t.Outcome + " in run " + t.Run
					}
					desc := t.Description
					if t.Internal {
						desc = "(internal) " + desc
					}
					fmt.Fprintf(a.stdout, "  %-20s %-40s %s\n", t.Name, desc, last)
				}
			}
			if len(st.Units) > 0 {
				fmt.Fprintln(a.stdout, "Units")
				for _, u := range st.Units {
					fmt.Fprintf(a.stdout, "  %-30s %-7s %s\n", u.Address, u.Kind, u.State)
				}
			}
			return nil
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
	return &cobra.Command{
		Use:   "runs [run]",
		Short: "List the runs of the stack, or show the events of one as it ran",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req, err := a.request(nil)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				// The events of the run, rendered as when it ran, or as
				// they are stored with --json.
				req.Run = args[0]
				return api.New(api.Handlers{Event: a.render()}).Events(context.Background(), req)
			}
			runs, err := a.api().Runs(context.Background(), req)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(runs)
			}
			for _, r := range runs {
				what := r.Command
				if r.Task != "" {
					what = r.Task
				}
				started := r.Started
				if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
					started = t.Format(time.RFC3339)
				}
				fmt.Fprintf(a.stdout, "%s  %-20s %-10s %s\n", r.Run, what, r.Outcome, started)
			}
			return nil
		},
	}
}
