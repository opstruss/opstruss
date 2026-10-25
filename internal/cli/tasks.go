package cli

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"context"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/sources"
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
	// A task of the project is one command; a task of units is one
	// command for every unit that has it, whatever its inputs there.
	defs := map[string][]*model.Task{}
	for name, t := range p.Tasks {
		if !t.Internal {
			defs[name] = []*model.Task{t}
		}
	}
	for _, path := range p.Order {
		for tn, t := range p.Nodes[path].Unit.Tasks {
			if p.Tasks[tn] == nil {
				defs[tn] = append(defs[tn], t)
			}
		}
	}
	// An instance of a component has the tasks of the component.
	for _, path := range slices.Sorted(maps.Keys(p.Nodes)) {
		if comp := p.Nodes[path].Component(); comp != nil {
			for tn, t := range comp.Tasks {
				if p.Tasks[tn] == nil && !t.Internal {
					defs[tn] = append(defs[tn], t)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		_, projectTask := p.Tasks[name]
		run.AddCommand(a.taskCommand(name, defs[name], !projectTask))
		if c, _, err := root.Find([]string{name}); err == nil && c != root {
			continue
		}
		if !root.ContainsGroup(groupTasks) {
			root.AddGroup(&cobra.Group{ID: groupTasks, Title: "Project tasks:"})
		}
		c := a.taskCommand(name, defs[name], !projectTask)
		c.GroupID = groupTasks
		root.AddCommand(c)
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
		case arg == "--stack" || arg == "--input" || arg == "--event":
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
	p, err := model.LoadWith(context.Background(), dir, stack, sources.Options{Discovery: true})
	if err != nil {
		a.loadErr = err
		return nil
	}
	a.discovery = p
	if p.Sources.Unavailable {
		a.loadErr = errors.New("remote component task metadata may be unavailable; run opstruss plugins fetch first")
	}
	return p
}

// taskCommand runs one task, of the project or of the units that have it:
// each input of those tasks is a flag, arrays repeat it, an object takes
// JSON or @file, a boolean also has --no-<name>, and an input with args:
// true also takes what comes after --. The engine reads the text of each
// flag by the type of the input in the task it reaches.
func (a *app) taskCommand(name string, tasks []*model.Task, ofUnits bool) *cobra.Command {
	var yes bool
	use := name
	if ofUnits {
		use += " [unit]..."
	}
	cmd := &cobra.Command{Use: use, Short: summary(tasks[0]), Annotations: map[string]string{detachable: "true"}}
	decls := map[string][]*model.Input{}
	for _, t := range tasks {
		for in, def := range t.Inputs {
			decls[in] = append(decls[in], def)
		}
	}
	all := func(in string, f func(*model.Input) bool) bool {
		return !slices.ContainsFunc(decls[in], func(d *model.Input) bool { return !f(d) })
	}
	typed := func(typ string) func(*model.Input) bool { return func(d *model.Input) bool { return d.Type == typ } }
	argsInput := ""
	sensitive := map[string]bool{}
	texts := map[string]*string{}
	lists := map[string]*[]string{}
	bools := map[string]*bool{}
	nots := map[string]*bool{}
	for _, in := range slices.Sorted(maps.Keys(decls)) {
		flag, help := model.FlagName(in), ""
		for _, d := range decls[in] {
			help = cmp.Or(help, d.Description)
		}
		if slices.ContainsFunc(decls[in], func(d *model.Input) bool { return d.Args }) {
			argsInput = in
		}
		switch {
		case slices.ContainsFunc(decls[in], func(d *model.Input) bool { return d.Sensitive }):
			// Only to say why it is not taken.
			sensitive[in] = true
			cmd.Flags().String(flag, "", "")
			_ = cmd.Flags().MarkHidden(flag)
		case all(in, typed("boolean")):
			bools[in], nots[in] = new(bool), new(bool)
			cmd.Flags().BoolVar(bools[in], flag, false, help)
			cmd.Flags().BoolVar(nots[in], "no-"+flag, false, "")
			cmd.MarkFlagsMutuallyExclusive(flag, "no-"+flag)
		case all(in, typed("array")):
			lists[in] = new([]string)
			cmd.Flags().StringArrayVar(lists[in], flag, nil, help)
		default:
			texts[in] = new(string)
			cmd.Flags().StringVar(texts[in], flag, "", help)
		}
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the changes of this run, as in CI")
	var event string
	if !ofUnits {
		cmd.Flags().StringVar(&event, "event", "", "run as a trigger of the task runs for this event: @<file>, @- for stdin, or JSON")
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		flags := map[string]any{}
		for _, in := range slices.Sorted(maps.Keys(decls)) {
			flag := model.FlagName(in)
			switch {
			case sensitive[in]:
				if c.Flags().Changed(flag) {
					return fmt.Errorf("--%s: input %s is sensitive, so it is not taken from a flag, which shell history and the list of processes keep; %s",
						flag, in, a.sensitiveHow(decls[in]))
				}
			case bools[in] != nil:
				// The value written counts: --name=false is false, and
				// --no-name=false is true.
				if c.Flags().Changed(flag) {
					flags[in] = *bools[in]
				} else if c.Flags().Changed("no-" + flag) {
					flags[in] = !*nots[in]
				}
			case lists[in] != nil && c.Flags().Changed(flag):
				list := []any{}
				for _, s := range *lists[in] {
					list = append(list, s)
				}
				flags[in] = list
			case texts[in] != nil && c.Flags().Changed(flag):
				text := *texts[in]
				if all(in, typed("object")) && strings.HasPrefix(text, "@") {
					var err error
					if text, err = a.objectFile(strings.TrimPrefix(text, "@")); err != nil {
						return fmt.Errorf("--%s: %w", flag, err)
					}
				}
				flags[in] = text
			}
		}
		units := args
		if dash := c.ArgsLenAtDash(); dash >= 0 {
			units = args[:dash]
			if argsInput == "" {
				return fmt.Errorf("task %s has no input with args: true for what comes after --", name)
			}
			if f := model.FlagName(argsInput); c.Flags().Changed(f) {
				return fmt.Errorf("input %s comes from --%s or from what follows --, not both", argsInput, f)
			}
			rest := args[dash:]
			if all(argsInput, typed("array")) {
				list := []any{}
				for _, s := range rest {
					list = append(list, s)
				}
				flags[argsInput] = list
			} else {
				flags[argsInput] = strings.Join(rest, " ")
			}
		}
		if len(units) > 0 && !ofUnits {
			return fmt.Errorf("task %s of the project takes no units", name)
		}
		req, err := a.request(units)
		if err != nil {
			return err
		}
		req.Yes, req.Task, req.Flags = yes, name, flags
		if c.Flags().Changed("event") {
			if req.Event, err = a.readEvent(event); err != nil {
				return fmt.Errorf("--event: %w", err)
			}
		}
		in := a.interrupts()
		defer in.stop()
		res, err := a.call(in.ctx, "run", req)
		if err != nil {
			return err
		}
		return in.exit(res.ExitCode)
	}
	return cmd
}

// readEvent reads the event of --event: @<file>, relative to the directory
// OpsTruss works in, @- for stdin, which then answers no question of the
// run, or the JSON itself; one JSON object of at most engine.MaxEvent
// bytes, which it does not quote back, as it may hold secrets.
func (a *app) readEvent(s string) (map[string]any, error) {
	var r io.Reader = strings.NewReader(s)
	switch {
	case s == "@-":
		if a.stdin == nil {
			return nil, errors.New("@- reads stdin, which this process has not")
		}
		r, a.stdin = a.stdin, nil
	case strings.HasPrefix(s, "@"):
		f, err := os.Open(joinDir(a.dir, s[1:]))
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, engine.MaxEvent+1))
	if err != nil {
		return nil, err
	}
	if len(data) > engine.MaxEvent {
		return nil, fmt.Errorf("the event is larger than %d MiB", engine.MaxEvent>>20)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var ev map[string]any
	if err := dec.Decode(&ev); err != nil || ev == nil {
		return nil, errors.New("the event is not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("the event holds more than one JSON value")
	}
	return ev, nil
}

// sensitiveHow says where a sensitive input comes from instead of a flag.
func (a *app) sensitiveHow(decls []*model.Input) string {
	how := "answer at the terminal"
	for _, d := range decls {
		if d.Env != "" {
			return "set $" + d.Env + ", or " + how
		}
	}
	return how
}

// objectFile reads the JSON or YAML object in the file at path, relative
// to the directory OpsTruss works in, as JSON for the engine to read.
func (a *app) objectFile(path string) (string, error) {
	data, err := os.ReadFile(joinDir(a.dir, path))
	if err != nil {
		return "", err
	}
	v, err := expr.FromYAML(data)
	if err != nil {
		return "", err
	}
	if _, ok := v.(map[string]any); !ok {
		return "", fmt.Errorf("@%s holds no object", path)
	}
	return expr.JSON(v), nil
}

// summary is the line of a task in help: its description, or without one
// what it runs, as npm run lists scripts with their commands: the first
// line of run, the ids of its steps, or the task it calls.
func summary(t *model.Task) string {
	text := t.Description
	switch {
	case text != "":
	case t.Run != nil && t.Run.Script != "":
		line, _, _ := strings.Cut(strings.TrimSpace(t.Run.Script), "\n")
		text = "run: " + line
	case t.Run != nil:
		text = "run: " + strings.Join(t.Run.Argv, " ")
	case len(t.Steps) > 0:
		var names []string
		for i, s := range t.Steps {
			names = append(names, cmp.Or(s.ID, s.Name, s.Task, strconv.Itoa(i+1)))
		}
		text = "steps: " + strings.Join(names, ", ")
	default:
		text = strings.Join(slices.Sorted(maps.Keys(t.Calls)), ", ")
	}
	if r := []rune(text); len(r) > 50 {
		text = string(r[:49]) + "…"
	}
	return text
}
