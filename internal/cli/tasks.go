package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/spf13/cobra"
)

// taskCommands makes a command of every task of the project that is not
// internal, and of every task of its units, also under opstruss run, which
// still reaches a task that a later built-in command shadows.
func (a *app) taskCommands(root *cobra.Command) {
	run := &cobra.Command{Use: "run <task>", Short: "Run a task of the project, also one that a built-in command shadows"}
	root.AddCommand(run)
	// A built-in command needs no task commands, so the project is read
	// for them only when the command may be a task, or for completion.
	if name := a.command(); name != "" && name != "run" && name != "help" && !a.completing() {
		if c, _, err := root.Find([]string{name}); err == nil && c != root {
			return
		}
	}
	p := a.commandProject()
	if p == nil {
		return
	}
	defs := map[string]*model.Task{}
	for name, t := range p.Tasks {
		if !t.Internal {
			defs[name] = t
		}
	}
	for _, path := range p.Order {
		for tn, t := range p.Nodes[path].Unit.Tasks {
			if defs[tn] == nil {
				defs[tn] = t
			}
		}
	}
	// An instance of a component has the tasks of the component.
	for _, path := range slices.Sorted(maps.Keys(p.Nodes)) {
		if comp := p.Nodes[path].Component(); comp != nil {
			for tn, t := range comp.Tasks {
				if defs[tn] == nil && !t.Internal {
					defs[tn] = t
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		_, unitTask := p.Tasks[name]
		run.AddCommand(a.taskCommand(name, defs[name], !unitTask))
		if c, _, err := root.Find([]string{name}); err == nil && c != root {
			continue
		}
		root.AddCommand(a.taskCommand(name, defs[name], !unitTask))
	}
}

// command is the first argument that is not a flag, which names the
// command.
func (a *app) command() string {
	for i := 0; i < len(a.args); i++ {
		arg := a.args[i]
		switch {
		case arg == "--":
			return ""
		case arg == "--stack" || arg == "--input":
			i++
		case strings.HasPrefix(arg, "-"):
		default:
			return arg
		}
	}
	return ""
}

func (a *app) completing() bool {
	for _, arg := range a.args {
		if arg == "--completion" || strings.HasPrefix(arg, "--completion=") || arg == "__complete" {
			return true
		}
	}
	return false
}

// commandProject reads the project to build the commands of its tasks, with
// the stack of --stack or the first one, since the commands are made before
// flags are parsed.
func (a *app) commandProject() *model.Project {
	dir, err := a.projectDir()
	if err != nil {
		return nil
	}
	stack := a.stackName()
	if stack == "" {
		if stacks, err := model.Stacks(dir); err == nil && len(stacks) > 0 {
			stack = stacks[0]
		}
	}
	p, err := model.Load(dir, stack)
	if err != nil {
		a.loadErr = err
		return nil
	}
	return p
}

// taskCommand runs one task: each input is a flag, arrays repeat it, an
// object takes JSON or @file, a boolean also has --no-<name>, and an input
// with args: true takes what comes after --.
func (a *app) taskCommand(name string, t *model.Task, ofUnits bool) *cobra.Command {
	var yes bool
	values := map[string]*string{}
	lists := map[string]*[]string{}
	bools := map[string]*bool{}
	nots := map[string]*bool{}
	use := name
	if ofUnits {
		use += " [unit]..."
	}
	cmd := &cobra.Command{Use: use, Short: t.Description}
	argsInput := ""
	secrets := map[string]*string{}
	for _, in := range slices.Sorted(maps.Keys(t.Inputs)) {
		def := t.Inputs[in]
		flag := strings.ReplaceAll(in, "_", "-")
		switch {
		case def.Sensitive:
			// Only to say why it is not taken.
			secrets[in] = new(string)
			cmd.Flags().StringVar(secrets[in], flag, "", "")
			_ = cmd.Flags().MarkHidden(flag)
		case def.Args:
			argsInput = in
		case def.Type == "boolean":
			bools[in], nots[in] = new(bool), new(bool)
			cmd.Flags().BoolVar(bools[in], flag, false, def.Description)
			cmd.Flags().BoolVar(nots[in], "no-"+flag, false, "")
		case def.Type == "array":
			lists[in] = new([]string)
			cmd.Flags().StringArrayVar(lists[in], flag, nil, def.Description)
		default:
			values[in] = new(string)
			cmd.Flags().StringVar(values[in], flag, "", def.Description)
		}
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the changes of this run, as in CI")
	cmd.RunE = func(c *cobra.Command, args []string) error {
		given := map[string]any{}
		set := func(in, flag string, parse func() (any, error)) error {
			if !c.Flags().Changed(flag) {
				return nil
			}
			v, err := parse()
			if err != nil {
				return fmt.Errorf("--%s: %w", flag, err)
			}
			given[in] = v
			return nil
		}
		for in, def := range t.Inputs {
			flag := strings.ReplaceAll(in, "_", "-")
			var err error
			switch {
			case secrets[in] != nil && c.Flags().Changed(flag):
				how := "answer at the terminal"
				if def.Env != "" {
					how = "set $" + def.Env + ", or " + how
				}
				err = fmt.Errorf("--%s: input %s is sensitive, so it is not taken from a flag, which shell history and the list of processes keep; %s", flag, in, how)
			case bools[in] != nil:
				err = set(in, flag, func() (any, error) { return true, nil })
				if err == nil {
					err = set(in, "no-"+flag, func() (any, error) { return false, nil })
				}
			case lists[in] != nil:
				err = set(in, flag, func() (any, error) {
					var items []any
					for _, s := range *lists[in] {
						typ := ""
						if def.Items != nil {
							typ = def.Items.Type
						}
						v, err := model.ParseText(s, typ)
						if err != nil {
							return nil, err
						}
						items = append(items, v)
					}
					return items, nil
				})
			case values[in] != nil:
				err = set(in, flag, func() (any, error) { return a.parseFlag(*values[in], def.Type) })
			}
			if err != nil {
				return err
			}
		}
		units := args
		if dash := c.ArgsLenAtDash(); dash >= 0 {
			units = args[:dash]
			if argsInput == "" {
				return fmt.Errorf("task %s has no input with args: true for what comes after --", name)
			}
			rest := args[dash:]
			if t.Inputs[argsInput].Type == "array" {
				list := []any{}
				for _, s := range rest {
					list = append(list, s)
				}
				given[argsInput] = list
			} else {
				given[argsInput] = strings.Join(rest, " ")
			}
		}
		if len(units) > 0 && !ofUnits {
			return fmt.Errorf("task %s of the project takes no units", name)
		}
		req, err := a.request(units)
		if err != nil {
			return err
		}
		req.Yes, req.Task, req.With = yes, name, given
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res, err := a.api().Run(ctx, req)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return &exitError{res.ExitCode}
		}
		return nil
	}
	return cmd
}

// parseFlag reads the text of a flag by the type of its input; an object
// is JSON, or @<file> with JSON or YAML.
func (a *app) parseFlag(s, typ string) (any, error) {
	if typ == "object" && strings.HasPrefix(s, "@") {
		data, err := os.ReadFile(joinDir(a.dir, strings.TrimPrefix(s, "@")))
		if err != nil {
			return nil, err
		}
		v, err := expr.FromYAML(data)
		if _, ok := v.(map[string]any); err == nil && !ok {
			return nil, fmt.Errorf("%s holds no object", s)
		}
		return v, err
	}
	return model.ParseText(s, typ)
}
