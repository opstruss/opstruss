// Package cli implements the opstruss command line.
package cli

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"context"

	"github.com/opstruss/opstruss/internal/ansi"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
	"golang.org/x/term"
)

// Main runs the command line with the process arguments and returns the
// exit code. A terminal on stdin can answer the questions of a run.
func Main() int {
	return start(os.Args[1:], os.Stdin, isTerminal(os.Stdin), os.Stdout, os.Stderr, os.Environ())
}

// isTerminal reports whether f is a terminal. /dev/null is a character
// device too, which CI and service managers give as stdin, and answers
// nothing.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// writesTerminal reports whether w is a terminal.
func writesTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}

// colorFor says whether what goes to w is in color (ansi.Color); with
// --json, only the environment turns it on, for a reader that shows ANSI.
func (a *app) colorFor(w io.Writer) bool {
	get := func(k string) string { return a.env[k] }
	if a.json {
		on, _ := ansi.Forced(get)
		return on
	}
	return ansi.Color(get, writesTerminal(w))
}

type app struct {
	locked    bool
	discovery *model.Project

	stdout, stderr io.Writer
	args           []string
	stdin          *bufio.Reader
	in             io.Reader // stdin as it is, for a program that opstruss exec attaches
	terminal       bool
	environ        []string
	env            map[string]string

	cwd        string // where OpsTruss started, before -C
	loadErr    error  // why the project's tasks are not commands
	dir        string
	detach     bool // -d: run in the daemon of the stack, started when none runs
	follow     bool // -f: logs goes on with what is written
	stack      string
	inputs     []string
	json       bool
	completion string
	renderMu   sync.Mutex
	renderers  map[io.Writer]*renderer // by where they write

	// While a question waits at the terminal, the output of the run waits
	// for its answer; past maxHeld only the record keeps it.
	asking    bool
	held      []heldText
	heldBytes int
	heldOver  int    // lines left out
	heldRun   string // the run whose record has them
}

// exitError ends a command that already wrote why it failed.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// run runs the command line with args in the operator's environment,
// without a terminal.
func run(args []string, stdout, stderr io.Writer, environ []string) int {
	return start(args, nil, false, stdout, stderr, environ)
}

func start(args []string, stdin io.Reader, terminal bool, stdout, stderr io.Writer, environ []string) int {
	a := &app{stdout: stdout, stderr: stderr, terminal: terminal, environ: environ, env: map[string]string{}}
	if stdin != nil {
		a.in, a.stdin = stdin, bufio.NewReader(stdin)
	}
	a.cwd, _ = os.Getwd()
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			a.env[k] = v
		}
	}
	args, err := a.takeShort(args)
	a.args = args
	if err == nil {
		root := a.root()
		defer func() {
			if a.discovery != nil {
				a.discovery.Close()
			}
		}()
		root.SetArgs(args)
		root.SetOut(stdout)
		root.SetErr(stderr)
		err = root.Execute()
		a.endOutput()
	}
	var exit *exitError
	var probs model.Problems
	// An unknown command may be a task of a project that does not load.
	if err != nil && strings.HasPrefix(err.Error(), "unknown command") && a.loadErr != nil {
		err = a.loadErr
	}
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	case errors.As(err, &probs):
		for _, p := range probs {
			fmt.Fprintln(stderr, p)
		}
	default:
		fmt.Fprintf(stderr, "opstruss: %v\n", err)
	}
	return 1
}

// takeShort removes from args the short flags that are OpsTruss's on every
// command line, since inputs make only long flags: -C <dir>, like git -C,
// each one after the first relative to the one before, -d, and -f.
func (a *app) takeShort(args []string) ([]string, error) {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return append(out, args[i:]...), nil
		case arg == "-C":
			if i+1 == len(args) {
				return nil, errors.New("-C needs a directory")
			}
			i++
			a.dir = joinDir(a.dir, args[i])
		case strings.HasPrefix(arg, "-C"):
			a.dir = joinDir(a.dir, strings.TrimPrefix(arg, "-C"))
		case arg == "-d":
			a.detach = true
		case arg == "-f":
			a.follow = true
		default:
			// The commands of tasks come from the project of the stack, so
			// --stack is read before the flags are.
			if arg == "--stack" && i+1 < len(args) {
				a.stack = args[i+1]
			} else if v, ok := strings.CutPrefix(arg, "--stack="); ok {
				a.stack = v
			}
			out = append(out, arg)
		}
	}
	return out, nil
}

func (a *app) lookupEnv(name string) (string, bool) {
	v, ok := a.env[name]
	return v, ok
}

// givenInputs reads --input name=value.
func (a *app) givenInputs() (map[string]string, error) {
	given := map[string]string{}
	for _, kv := range a.inputs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--input %s is not name=value", kv)
		}
		given[k] = v
	}
	return given, nil
}

func joinDir(base, dir string) string {
	if base == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(base, dir)
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:                        "opstruss",
		Short:                      "Event-driven workflow orchestration that runs any tool and hands its typed results to the next.",
		SuggestionsMinimumDistance: 2,
		// cobra.NoArgs would leave out the suggestions of an unknown
		// command, which git and kubectl give.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
			if near := cmd.SuggestionsFor(args[0]); len(near) > 0 {
				msg += "; did you mean " + strings.Join(near, " or ") + "?"
			}
			return errors.New(msg)
		},
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.completion != "" {
				return a.printCompletion(cmd)
			}
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if a.detach && cmd.Annotations[detachable] == "" {
				return fmt.Errorf("-d is for up and tasks, which can start a daemon, not for %s", cmd.Name())
			}
			if a.follow && cmd.Name() != "logs" && cmd.Name() != "runs" {
				return fmt.Errorf("-f is for logs and runs <run>, which follow what is written, not for %s", cmd.Name())
			}
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&a.stack, "stack", "", "the stack to use; defaults to the only one, or $OPSTRUSS_STACK")
	pf.StringArrayVar(&a.inputs, "input", nil, "set a project input, as name=value")
	pf.BoolVar(&a.locked, "locked", false, "require existing dependency pins without changing opstruss.lock")
	pf.BoolVar(&a.json, "json", false, "print JSON")
	root.Flags().StringVar(&a.completion, "completion", "", "print a completion script for bash, zsh, or fish")
	root.AddCommand(a.versionCommand(), a.initCommand(), a.validateCommand(), a.explainCommand(), a.stateCommand())
	root.AddCommand(a.unitCommands()...)
	root.AddCommand(a.listCommand(), a.outputsCommand(), a.runsCommand(), a.execCommand(), a.doctorCommand(),
		a.stopCommand(), a.unlockCommand(), a.daemonCommand(), a.pluginsCommand())
	// Help shows the commands of OpsTruss apart from the tasks of the
	// project, as npm run and just --list show scripts on their own.
	root.AddGroup(&cobra.Group{ID: groupCommands, Title: "Commands:"})
	root.SetHelpCommandGroupID(groupCommands)
	a.taskCommands(root)
	for _, c := range root.Commands() {
		if c.GroupID == "" {
			c.GroupID = groupCommands
		}
	}
	return root
}

// The groups of commands in help.
const (
	groupCommands = "commands"
	groupTasks    = "tasks"
)

func (a *app) printCompletion(root *cobra.Command) error {
	switch a.completion {
	case "bash":
		return root.GenBashCompletionV2(a.stdout, true)
	case "zsh":
		return root.GenZshCompletion(a.stdout)
	case "fish":
		return root.GenFishCompletion(a.stdout, true)
	}
	return fmt.Errorf("--completion takes bash, zsh, or fish, not %q", a.completion)
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.json {
				return a.printJSON(map[string]string{"version": version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
			}
			fmt.Fprintf(a.stdout, "opstruss %s %s/%s\n", version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// version returns the module version stamped by the go command: the tag for
// release builds and go install, a pseudo-version for builds in a checkout.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// projectDir finds the project from the directory OpsTruss starts in.
func (a *app) projectDir() (string, error) {
	return model.Find(cmp.Or(a.dir, "."))
}

// stackName is --stack, or $OPSTRUSS_STACK outside a run, because a run
// started by a program of another run chooses its stack itself.
func (a *app) stackName() string {
	if a.stack != "" {
		return a.stack
	}
	if _, nested := a.lookupEnv("OPSTRUSS_RUN"); nested {
		return ""
	}
	s, _ := a.lookupEnv("OPSTRUSS_STACK")
	return s
}

func (a *app) validateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the project's files without running anything",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := a.projectDir()
			if err != nil {
				return err
			}
			err = model.ValidateWith(context.Background(), dir, sources.Options{Locked: a.locked})
			var probs model.Problems
			if err != nil && !errors.As(err, &probs) {
				return err
			}
			if a.json {
				if probs == nil {
					probs = model.Problems{}
				}
				if err := a.printJSON(map[string]any{"valid": len(probs) == 0, "problems": probs}); err != nil {
					return err
				}
				if len(probs) > 0 {
					return &exitError{1}
				}
				return nil
			}
			if len(probs) > 0 {
				return probs
			}
			fmt.Fprintln(a.stdout, "valid")
			return nil
		},
	}
}

func (a *app) explainCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "explain [unit|task]...",
		Short: "Print the project of one stack as read and resolved, without state",
		RunE: func(_ *cobra.Command, names []string) error {
			dir, err := a.projectDir()
			if err != nil {
				return err
			}
			p, err := model.LoadWith(context.Background(), dir, a.stackName(), sources.Options{Locked: a.locked})
			if err != nil {
				return err
			}
			defer p.Close()
			given, err := a.givenInputs()
			if err != nil {
				return err
			}
			red := secret.NewRedactor()
			inputs, err := p.ResolveInputs(given, a.lookupEnv, func(_ string, v any) { red.Add(v) })
			if err != nil {
				return red.Err(err)
			}
			values, marks := map[string]any{}, map[string]any{}
			for k, v := range inputs {
				values[k] = v.Value
				if p.Inputs[k].Sensitive {
					marks[k] = true
					v.Value = secret.Mask
					inputs[k] = v
				}
			}
			locals, err := p.EvalLocals(values, nil, nil)
			if err != nil {
				return red.Err(err)
			}
			for k := range p.LocalMarks(map[string]any{"inputs": marks}, nil) {
				locals[k] = secret.Mask
			}
			out, err := explain(p, inputs, locals, names)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(out)
			}
			enc := yaml.NewEncoder(a.stdout)
			enc.SetIndent(2)
			if err := enc.Encode(out); err != nil {
				return err
			}
			return enc.Close()
		},
	}
}

// explain builds the document that explain prints: the stack's project as
// written, each input's value and where it came from, the locals, and the
// order of the units. Names pick units and tasks, with what they depend on.
func explain(p *model.Project, inputs map[string]model.InputValue, locals map[string]any, names []string) (map[string]any, error) {
	out := map[string]any{"project": p.Name, "stack": p.StackName, "inputs": inputs, "order": p.Order}
	for k, v := range p.Doc() {
		switch k {
		case "name", "description", "opstruss", "inputs":
		default:
			out[k] = v
		}
	}
	if len(locals) > 0 {
		out["locals"] = locals
	}
	if len(names) == 0 {
		return out, nil
	}
	units, _ := out["units"].(map[string]any)
	tasks, _ := out["tasks"].(map[string]any)
	pickedUnits, pickedTasks := map[string]any{}, map[string]any{}
	var addUnit func(string)
	addUnit = func(n string) {
		if _, done := pickedUnits[n]; done {
			return
		}
		pickedUnits[n] = units[n]
		for _, leaf := range p.Leaves(n) {
			for _, d := range p.Deps[leaf] {
				root, _, _ := strings.Cut(d, "/")
				addUnit(root)
			}
		}
	}
	var addTask func(string)
	addTask = func(n string) {
		if _, done := pickedTasks[n]; done {
			return
		}
		pickedTasks[n] = tasks[n]
		for _, d := range p.Tasks[n].Needs {
			addTask(d)
		}
	}
	for _, n := range names {
		_, isUnit := units[n]
		_, isTask := tasks[n]
		if !isUnit && !isTask {
			return nil, fmt.Errorf("the project has no unit or task %s; it has %s", n,
				strings.Join(slices.Sorted(slices.Values(append(slices.Collect(maps.Keys(units)), slices.Collect(maps.Keys(tasks))...))), ", "))
		}
		if isUnit {
			addUnit(n)
		}
		if isTask {
			addTask(n)
		}
	}
	out["units"], out["tasks"] = pickedUnits, pickedTasks
	out["order"] = slices.DeleteFunc(slices.Clone(p.Order), func(u string) bool {
		root, _, _ := strings.Cut(u, "/")
		return !hasUnit(pickedUnits, root)
	})
	return out, nil
}

func hasUnit(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
