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
	"time"

	"context"

	"github.com/opstruss/opstruss/internal/ansi"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	live      bool   // the output goes on above the question at the terminal
	prompt    string // the question that waits, to write again under the output
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
	var ran *cobra.Command
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
		ran, err = root.ExecuteC()
		a.endOutput()
	}
	var exit *exitError
	var probs model.Problems
	// An unknown command may be a task of a project that does not load.
	if err != nil && strings.HasPrefix(err.Error(), "unknown command") && a.loadErr != nil {
		err = a.loadErr
	}
	if err != nil && a.json && !errors.As(err, &exit) {
		a.jsonError(ran, err)
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

// eventCommands print the events of a run with --json; the other commands
// print one document.
var eventCommands = []string{"plan", "up", "destroy", "status", "logs", "rerun"}

// jsonError prints, with --json, what failed before cmd printed anything:
// for a command that prints events, a diagnostic event for each problem of
// the files and a last run event with its exit code, as terraform plan
// -json prints the diagnostics of its configuration; for the others, a
// document with the error. stderr still has the text.
func (a *app) jsonError(cmd *cobra.Command, err error) {
	events := cmd != nil && (slices.Contains(eventCommands, cmd.Name()) || cmd.GroupID == groupTasks ||
		cmd.HasParent() && cmd.Parent().Name() == "run")
	if !events {
		_ = a.printJSON(map[string]string{"error": err.Error()})
		return
	}
	var msgs []string
	var probs model.Problems
	if errors.As(err, &probs) {
		for _, p := range probs {
			msgs = append(msgs, p.String())
		}
	} else {
		msgs = append(msgs, err.Error())
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	seq := int64(0)
	for _, m := range msgs {
		seq++
		_ = enc.Encode(record.Event{V: 1, Seq: seq, Time: now, Type: "diagnostic", Level: "error", Message: m})
	}
	code := 1
	_ = enc.Encode(record.Event{V: 1, Seq: seq + 1, Time: now, Type: "run", Level: "error", Message: "failed before it started",
		Status: record.Finished, Outcome: "failure", ExitCode: &code})
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

// unknownCommand is the error of a name that is no command of cmd, with
// the suggestions that git and kubectl give, which cobra.NoArgs would leave
// out.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if near := cmd.SuggestionsFor(args[0]); len(near) > 0 {
		msg += "; did you mean " + strings.Join(near, " or ") + "?"
	}
	return errors.New(msg)
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:                        "opstruss",
		Short:                      "Event-driven workflow orchestration that runs any tool and hands its typed results to the next.",
		SuggestionsMinimumDistance: 2,
		Args:                       unknownCommand,
		SilenceErrors:              true,
		SilenceUsage:               true,
		CompletionOptions:          cobra.CompletionOptions{DisableDefaultCmd: true},
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
	root.Flags().BoolP("version", "v", false, "print the version, as opstruss version does")
	// Help lists the commands in the order that a project meets them.
	units := a.unitCommands()
	root.AddCommand(a.initCommand(), a.validateCommand())
	root.AddCommand(units...)
	root.AddCommand(a.listCommand(), a.outputsCommand(), a.runsCommand(), a.explainCommand(), a.doctorCommand(),
		a.execCommand(), a.pluginsCommand(), a.stateCommand(), a.stopCommand(), a.unlockCommand(), a.versionCommand(),
		a.daemonCommand())
	a.completions(root)
	root.SetUsageTemplate(usageTemplate(root.UsageTemplate()))
	root.Version = version()
	root.SetVersionTemplate(`opstruss {{.Version}} ` + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	// Help shows the commands that a project starts with first, as
	// terraform -help shows its main commands, and the commands of
	// OpsTruss apart from the tasks of the project, as npm run and just
	// --list show scripts on their own.
	root.AddGroup(&cobra.Group{ID: groupMain, Title: "Main commands:"},
		&cobra.Group{ID: groupInspect, Title: "Inspect:"},
		&cobra.Group{ID: groupCommands, Title: "Other commands:"})
	root.SetHelpCommandGroupID(groupCommands)
	a.taskCommands(root)
	for _, c := range root.Commands() {
		if c.GroupID == "" {
			c.GroupID = cmp.Or(commandGroups[c.Name()], groupCommands)
		}
	}
	return root
}

// ShortFlag is a flag that takeShort reads before cobra does: a flag of
// cobra has a long name, and long names belong to the inputs of tasks.
type ShortFlag struct {
	Name, Arg string
	Usage     string // with Arg in backquotes, as pflag reads it
	Global    bool   // on every command; else on the detachable ones
}

// For reports whether f is a flag of cmd.
func (f ShortFlag) For(cmd *cobra.Command) bool {
	return f.Global || cmd.Annotations[detachable] != ""
}

// shortFlags are the short flags, which help and the reference print.
var shortFlags = []ShortFlag{
	{"C", "dir", "run as if OpsTruss started in `dir`; a -C after another is relative to it", true},
	{"d", "", "run in the daemon of the stack, which starts in the background when none runs", false},
}

// Reference is the command tree of opstruss without the tasks of a
// project, and its short flags, for the reference of the documentation.
func Reference() (*cobra.Command, []ShortFlag) {
	// A built-in command as the command line reads no project.
	a := &app{args: []string{"version"}, env: map[string]string{}}
	return a.root(), shortFlags
}

func init() {
	cobra.EnableCommandSorting = false
	cobra.AddTemplateFunc("flagUsages", flagUsages)
	cobra.AddTemplateFunc("docsURL", func(cmd *cobra.Command) string { return docsURL(version(), docsPath(cmd)) })
}

// usageTemplate is cobra's usage template with the flags of each section
// printed by flagUsages, and each help ending with its page in the
// documentation of this version, as gh ends each help with its manual.
func usageTemplate(t string) string {
	t = strings.Replace(t, "{{.LocalFlags.FlagUsages", "{{flagUsages . .LocalFlags true", 1)
	t = strings.Replace(t, "{{.InheritedFlags.FlagUsages", "{{flagUsages . .InheritedFlags false", 1)
	return strings.TrimSuffix(t, "\n") + "\n\nDocumentation: {{docsURL .}}\n"
}

// docsPath is the page of cmd in the documentation: its reference, or the
// start for opstruss itself and the tasks of a project.
func docsPath(cmd *cobra.Command) string {
	if !cmd.HasParent() || cmd.GroupID == groupTasks {
		return ""
	}
	return "reference/cli/" + strings.ReplaceAll(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "), " ", "-")
}

// flagUsages is fs.FlagUsages with the short flags that belong in it: -C
// where --stack is, and -d in the flags of a detachable command. pflag
// prints a flag only with its long name, so a short flag is printed as a
// long one and its name rewritten at the same width, which keeps the
// columns.
func flagUsages(cmd *cobra.Command, fs *pflag.FlagSet, local bool) string {
	show := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	show.AddFlagSet(fs)
	long := map[string]string{}
	for _, f := range shortFlags {
		if !f.For(cmd) || f.Global && fs.Lookup("stack") == nil || !f.Global && !local {
			continue
		}
		name := "-" + f.Name
		if f.Arg != "" {
			show.String(f.Name, "", f.Usage)
			name += " " + f.Arg
		} else {
			show.Bool(f.Name, false, f.Usage)
		}
		long["      -"+name+" "] = "  " + name + "      "
	}
	lines := strings.SplitAfter(show.FlagUsages(), "\n")
	for i, l := range lines {
		for from, to := range long {
			if rest, ok := strings.CutPrefix(l, from); ok {
				lines[i] = to + rest
			}
		}
	}
	return strings.Join(lines, "")
}

// The groups of commands in help.
const (
	groupMain     = "main"
	groupInspect  = "inspect"
	groupCommands = "commands"
	groupTasks    = "tasks"
)

// commandGroups are the groups of help other than the other commands.
var commandGroups = map[string]string{
	"init": groupMain, "validate": groupMain, "plan": groupMain, "up": groupMain, "destroy": groupMain,
	"status": groupInspect, "list": groupInspect, "outputs": groupInspect, "logs": groupInspect,
	"runs": groupInspect, "explain": groupInspect, "doctor": groupInspect,
}

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
// completions suggests the units of the project to the commands that take
// units, its stacks to --stack, and its newest runs to the commands that
// take a run; the project as the task commands read it, so a project that
// cannot be read suggests nothing.
func (a *app) completions(root *cobra.Command) {
	none := cobra.ShellCompDirectiveNoFileComp
	units := func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		if a.discovery == nil {
			return nil, none
		}
		return a.discovery.Order, none
	}
	for _, path := range [][]string{{"plan"}, {"up"}, {"destroy"}, {"status"}, {"logs"}, {"exec"}} {
		if c, _, err := root.Find(path); err == nil && c != root {
			c.ValidArgsFunction = units
		}
	}
	_ = root.RegisterFlagCompletionFunc("stack", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		dir, err := a.projectDir()
		if err != nil {
			return nil, none
		}
		stacks, _ := model.Stacks(dir)
		return stacks, none
	})
	runs := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if a.discovery == nil || len(args) > 0 {
			return nil, none
		}
		ents, _ := os.ReadDir(filepath.Join(a.discovery.Dir, ".opstruss", "runs", a.discovery.StackName))
		var ids []string
		for i := len(ents) - 1; i >= 0 && len(ids) < 20; i-- {
			if ents[i].IsDir() {
				ids = append(ids, ents[i].Name())
			}
		}
		return ids, none
	}
	for _, path := range [][]string{{"runs"}, {"runs", "cancel"}, {"runs", "rerun"}} {
		if c, _, err := root.Find(path); err == nil && c != root {
			c.ValidArgsFunction = runs
		}
	}
}

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
		Short: "Print the project of a stack as resolved",
		Long:  "Print the project of one stack as read and resolved, without state.",
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
	// A sops file is named as a run reads it, from what a plan knows,
	// without reading the secret.
	if secrets, ok := out["secrets"].(map[string]any); ok {
		secrets = maps.Clone(secrets)
		values := map[string]any{}
		for k, v := range inputs {
			values[k] = v.Value
		}
		vars := map[string]any{"stack": p.StackName, "inputs": values, "locals": locals}
		for name, src := range p.Secrets {
			doc, ok := secrets[name].(map[string]any)
			if !ok || !expr.HasExpr(src.Sops) {
				continue
			}
			if file, err := expr.EvalText(src.Sops, &expr.Scope{Dir: p.DirOf("secrets." + name), Vars: vars}); err == nil {
				doc = maps.Clone(doc)
				doc["sops"] = file
				secrets[name] = doc
			}
		}
		out["secrets"] = secrets
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
