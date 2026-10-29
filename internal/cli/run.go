package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/opstruss/opstruss/internal/api"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// unitCommands are the commands that change units: plan, up, and destroy.
// They have no aliases: the names that users of other tools reach for are
// only suggested, as git suggests a command for a typo.
func (a *app) unitCommands() []*cobra.Command {
	type flags struct{ yes, force, noDeps bool }
	command := func(use, short, force string, suggestFor ...string) *cobra.Command {
		var f flags
		cmd := &cobra.Command{
			Use:        use + " [unit]...",
			Short:      short,
			SuggestFor: suggestFor,
			RunE: func(_ *cobra.Command, units []string) error {
				req, err := a.request(units)
				if err != nil {
					return err
				}
				req.Yes, req.Force, req.NoDeps = f.yes, f.force, f.noDeps
				in := a.interrupts()
				defer in.stop()
				res, err := a.call(in.ctx, use, req)
				if err != nil {
					return err
				}
				// Without a terminal nothing could answer, so say what would,
				// as pulumi up does without --yes.
				if res.ExitCode == 2 && !req.Yes && !a.answers() {
					fmt.Fprintf(a.stderr, "changes need approval: run %s at a terminal, or with --yes\n", use)
				}
				return in.exit(res.ExitCode)
			},
		}
		cmd.Flags().BoolVar(&f.noDeps, "no-deps", false, "leave out the units that the ones named depend on, or for destroy those that depend on them")
		if force != "" {
			cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "approve the changes of this run, as in CI")
			cmd.Flags().BoolVar(&f.force, "force", false, force)
		}
		return cmd
	}
	up := command("up", "Bring the units up: each runs when its definition changed",
		"run the units named although unchanged", "apply", "deploy", "start")
	up.Long = up.Short + `.

With -d, up runs in the daemon of the stack, which starts in the background
when none runs and stays until opstruss stop. While a daemon runs, the runs
of the command line go to it. Without one, an up that starts a service makes
this process the daemon of the stack, which shows the output of its services
and stays while they run; Ctrl-C stops them.`
	up.Annotations = map[string]string{detachable: "true"}
	up.Example = `  opstruss up                 # plan, ask, and apply the stack
  opstruss up --stack prod -y # apply without asking, as in CI
  opstruss up web             # web and the units it reads
  opstruss up -d              # in the daemon, with its triggers and services`
	destroy := command("destroy", "Take the units of the stack down",
		"run destroy although pass values differ from those recorded", "down", "delete", "remove", "rm")
	destroy.Long = "Take the units of the stack down, each with the destroy program and inputs that its record keeps."
	destroy.Example = `  opstruss destroy --stack dev
  opstruss destroy web        # web and the units that read it`
	status := command("status", "Show the state of the units", "", "ps")
	status.Long = "Show the state of the units: the status task of their plugin, or what their record says."
	plan := command("plan", "Show what up would do and why, without changing anything", "", "preview", "diff")
	plan.Example = `  opstruss plan
  opstruss plan --stack prod --json`
	return []*cobra.Command{
		plan,
		up,
		destroy,
		status,
		a.logsCommand(),
	}
}

// logsCommand shows the output of the units. It runs in this process also
// while a daemon runs, since it only reads, so following a log does not
// keep a daemon in the foreground.
func (a *app) logsCommand() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [unit|task]...",
		Short: "Show the output of units and tasks",
		Example: `  opstruss logs              # every unit and task
  opstruss logs web -f       # follow the log of the service web
  opstruss logs backup       # the last run of the task backup`,
		Long: "Show the output of the units: the log of a service, the logs task of their plugin, or the last run that touched them; and of tasks, from their last run.",
		RunE: func(_ *cobra.Command, units []string) error {
			req, err := a.request(units)
			if err != nil {
				return err
			}
			req.Follow = follow || a.follow
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := a.api().Logs(ctx, req)
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return &exitError{res.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "go on with what services, plugins, and runs of the tasks write, until Ctrl-C; -f is the same")
	return cmd
}

// request is a call of the engine API about the project and stack of
// this command, with the units named.
func (a *app) request(units []string) (api.Request, error) {
	dir, err := a.projectDir()
	if err != nil {
		return api.Request{Locked: a.locked}, err
	}
	given, err := a.givenInputs()
	if err != nil {
		return api.Request{Locked: a.locked}, err
	}
	debug, _ := a.lookupEnv("OPSTRUSS_LOG")
	return api.Request{Locked: a.locked, Dir: dir, Cwd: a.cwd, Stack: a.stackName(), Inputs: given, Environ: a.environ, Units: units,
		Color: a.colorFor(a.stdout),
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
	if err := ctx.Err(); err != nil {
		// An interrupted run asks nothing more.
		return false, err
	}
	a.renderMu.Lock()
	r := a.renderers[a.stdout]
	shown := false
	if r != nil {
		for _, lines := range r.planned {
			shown = shown || lines[q.Address] == q.Summary
		}
	}
	a.renderMu.Unlock()
	items := q.Items
	if len(items) == 0 {
		items = []engine.Question{q}
	}
	// One answer approves every item, as terraform apply asks once: the
	// question names what yes does to each, in the order of the plan, and
	// the instances of one unit by their count, as the plan does.
	type ask struct {
		verb, who, unit string
		n               int
	}
	var groups []ask
	var plans []string
	for _, it := range items {
		who := cmp.Or(it.Address, it.Task)
		plans = append(plans, who+": "+it.Summary+"\n")
		if r != nil && r.terminal {
			who = unitName(who)
		}
		unit := who
		if i := strings.LastIndex(who, "["); i > strings.LastIndex(who, "/") {
			unit = who[:i]
		}
		v := askVerb(it)
		if k := len(groups) - 1; k >= 0 && unit != who && groups[k].unit == unit && groups[k].verb == v {
			groups[k].n++
			continue
		}
		groups = append(groups, ask{v, who, unit, 1})
	}
	var asks []string
	verb := ""
	for _, g := range groups {
		who := g.who
		if g.n > 1 {
			who = fmt.Sprintf("%d instances of %s", g.n, g.unit)
		}
		switch {
		case g.verb == verb:
			asks = append(asks, who)
		case verb == "":
			asks = append(asks, g.verb+" "+who)
		default:
			asks = append(asks, strings.ToLower(g.verb)+" "+who)
		}
		verb = g.verb
	}
	question := strings.Join(plans, "") + strings.Join(asks, ", ") + "? [y/N] "
	if r != nil && r.terminal {
		// The question names the units as the plan table does, in one
		// line, and says the plan of one only when it is not the one the
		// table showed.
		question = strings.Join(asks, ", ") + "? "
		if why := planText(engine.Event{Address: q.Address, Message: q.Summary}); len(q.Items) == 0 && !shown && q.Op != "task" && why != "destroy" {
			question += paint(a.colorFor(a.stderr), dim, "("+why+")") + " "
		}
		question += "[y/N] "
	}
	a.question(question)
	line, err := a.readLine(ctx)
	a.answered(err != nil)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// askVerb names what yes does to the change of q, as terraform destroy
// asks about destroying; a daemon older than op asks to apply.
func askVerb(q engine.Question) string {
	switch {
	case q.Op == "destroy" && strings.HasPrefix(q.Summary, "will be forgotten"):
		// Only its record goes.
		return "Forget"
	case q.Op == "destroy":
		return "Destroy"
	case q.Op == "task":
		return "Run"
	}
	return "Apply"
}

// ask asks at the terminal for a required input that has no value.
func (a *app) ask(ctx context.Context, name string, in *model.Input) (string, error) {
	prompt := name
	if in.Description != "" {
		prompt += " (" + in.Description + ")"
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.question(prompt + ": ")
	if f, ok := a.in.(*os.File); ok && in.Sensitive && term.IsTerminal(int(f.Fd())) {
		// A sensitive input is typed without echo, so the line ends here.
		b, err := term.ReadPassword(int(f.Fd()))
		a.answered(true)
		return string(b), err
	}
	line, err := a.readLine(ctx)
	a.answered(err != nil)
	return strings.TrimRight(line, "\r\n"), err
}

// maxHeld bounds the output that a question holds back.
const maxHeld = 1 << 20

// question writes text, which asks something at the terminal, and holds
// the output of the run back until the answer, so the question stays the
// last line there, as terraform apply asks with nothing else on screen.
func (a *app) question(text string) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	// The plan is done once the run asks about it.
	for _, r := range a.renderers {
		for _, p := range r.flushPlan() {
			a.write(p.w, p.text, "")
		}
	}
	a.asking = true
	// At a terminal, the output of the run goes on above the question, which
	// stays the last line, as prompt_toolkit prints above its prompt;
	// elsewhere the output waits for the answer.
	r := a.renderers[a.stdout]
	a.live = r != nil && r.terminal && writesTerminal(a.stderr) && !strings.Contains(text, "\n")
	a.prompt = text
	fmt.Fprint(a.stderr, text)
}

// answered writes the output that came while the question waited, and
// says how much of it, past maxHeld, only the record keeps.
// The line of a question that got no answer, as one Ctrl-C ended, ends
// with closeLine.
func (a *app) answered(closeLine bool) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	if closeLine && a.prompt != "" {
		fmt.Fprintln(a.stderr)
	}
	a.asking, a.live, a.prompt = false, false, ""
	for _, h := range a.held {
		_, _ = io.WriteString(h.w, h.text)
	}
	if a.heldOver > 0 {
		fmt.Fprintf(a.stderr, "%d more lines came while the question waited; opstruss runs %s shows them\n", a.heldOver, a.heldRun)
	}
	a.held, a.heldBytes, a.heldOver = nil, 0, 0
}

// write writes text to w, or holds it while a question waits. The caller
// holds renderMu.
func (a *app) write(w io.Writer, text, run string) {
	switch {
	case !a.asking:
		_, _ = io.WriteString(w, text)
	case a.live:
		// The question line gives way to the output and comes back under it.
		_, _ = io.WriteString(a.stderr, "\r\x1b[2K")
		_, _ = io.WriteString(w, text)
		if a.prompt != "" {
			_, _ = io.WriteString(a.stderr, a.prompt)
		}
	case a.heldBytes+len(text) > maxHeld:
		a.heldOver += strings.Count(text, "\n")
		a.heldRun = cmp.Or(run, a.heldRun)
	default:
		a.held = append(a.held, heldText{w, text})
		a.heldBytes += len(text)
	}
}

// heldText is output that a question holds back.
type heldText struct {
	w    io.Writer
	text string
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
		Short: "Run a program with the environment of a unit",
		Example: `  opstruss exec infra -- tofu state list
  opstruss exec web -- docker compose ps`,
		Long: "Run a program with what the programs of one unit get, under its lock, on this terminal.",
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

// render prints events to stdout, as renderer says.
func (a *app) render() func(engine.Event) { return a.renderTo(a.stdout) }

// renderTo is render with the events that are not warnings or errors on
// out.
func (a *app) renderTo(out io.Writer) func(engine.Event) {
	r := a.rendererFor(out)
	return func(ev engine.Event) {
		// Runs and services write at once, a line each.
		a.renderMu.Lock()
		defer a.renderMu.Unlock()
		for _, p := range r.render(ev) {
			a.write(p.w, p.text, ev.Run)
		}
	}
}

// rendererFor returns the one renderer of what the command writes to out.
func (a *app) rendererFor(out io.Writer) *renderer {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	if r := a.renderers[out]; r != nil {
		return r
	}
	r := newRenderer(out, a.stderr, a.json, writesTerminal(out) && a.env["TERM"] != "dumb", a.env["GITHUB_ACTIONS"] == "true",
		a.colorFor(out), a.colorFor(a.stderr))
	if a.renderers == nil {
		a.renderers = map[io.Writer]*renderer{}
	}
	a.renderers[out] = r
	return r
}

// endOutput ends what the renderers left open, such as a group in GitHub
// Actions, once the command is done.
func (a *app) endOutput() {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	for _, r := range a.renderers {
		for _, p := range r.end() {
			a.write(p.w, p.text, "")
		}
	}
}

// interrupts is how a command that runs stops on SIGINT and SIGTERM: the
// first cancels ctx with engine.Interrupted, so each program gets its stop
// signal and stop.grace and the run ends cancelled, and a second kills
// them at once, as Terraform stops on two interrupts.
type interrupts struct {
	ctx  context.Context
	stop func()
	mu   sync.Mutex
	got  syscall.Signal // the first signal
}

func (a *app) interrupts() *interrupts {
	ctx, cancel := context.WithCancelCause(context.Background())
	ctx, kill := proc.WithKill(ctx)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	in := &interrupts{ctx: ctx}
	done := make(chan struct{})
	go func() {
		for n := 0; ; n++ {
			select {
			case <-done:
				return
			case s := <-ch:
				switch n {
				case 0:
					sig, _ := s.(syscall.Signal)
					in.mu.Lock()
					in.got = sig
					in.mu.Unlock()
					a.notice("interrupted: nothing more starts, and the programs that run stop within their stop.grace; interrupt again to kill them at once")
					cancel(engine.Interrupted{Signal: sig})
				case 1:
					a.notice("two interrupts received: killing the programs at once; the next plan shows what did not finish")
					kill()
				}
			}
		}
	}()
	in.stop = func() {
		signal.Stop(ch)
		close(done)
		cancel(nil)
		kill()
	}
	return in
}

// exit ends a command whose run ended with code: with 128 and the signal
// when one stopped it, as a shell reports, otherwise with code.
func (in *interrupts) exit(code int) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.got != 0 {
		code = 128 + int(in.got)
	}
	if code != 0 {
		return &exitError{code}
	}
	return nil
}

// notice writes a line of OpsTruss itself to stderr, between the lines of
// events.
func (a *app) notice(msg string) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	terminal := writesTerminal(a.stderr)
	if terminal {
		msg = paint(a.colorFor(a.stderr), yellow, msg)
	}
	if a.asking && a.live && a.prompt != "" {
		// An interrupt ends the question: its line stays as it is, with
		// the ^C of the terminal, and the notice comes under it.
		a.prompt = ""
		_, _ = io.WriteString(a.stderr, "\n"+msg+"\n")
		return
	}
	if terminal {
		// The terminal wrote ^C at the start of a line; the notice comes
		// under it.
		msg = "\n" + msg
	}
	a.write(a.stderr, msg+"\n", "")
}
