package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
)

// Call is one program and what its kind of program receives.
type Call struct {
	Command model.Command
	Args    []string // after the arguments of Command, for the exec hook: the argv of opstruss exec
	Shell   string
	Base    string                    // directory of the file that defines the program
	Dir     string                    // working directory, relative to Base
	Env     map[string]model.EnvValue // env: of the unit, task, and step, resolved
	Pass    []string                  // names and patterns of the operator's variables to pass
	Stdin   []byte                    // nil reads nothing
	Stop    Stop

	Task   string         // OPSTRUSS_TASK
	Reason string         // OPSTRUSS_REASON
	Digest string         // OPSTRUSS_DIGEST, for up
	Unit   *Unit          // for the programs of a unit
	Inputs map[string]any // INPUT_* and OPSTRUSS_INPUTS, for the program of a task or plugin
	Plan   bool           // asks for OPSTRUSS_PLAN

	// PluginDir is the directory of the plugin whose program this is:
	// OPSTRUSS_PLUGIN_DIR, and where a relative program in argv starts.
	PluginDir  string
	BinDirs    []string
	LeaseFiles []*os.File
	// AnyOutputs keeps every output the program writes, for a plugin with
	// outputs: tool, whose user's code names them.
	AnyOutputs bool
	// Secret marks the program of a secret source, which gets no
	// OPSTRUSS_ARTIFACTS, where the secret would stay with the run.
	Secret bool

	Outputs map[string]*model.Output // a stream that an output reads stays out of the log
	Stdout  io.Writer                // live log of the streams that no output reads
	Stderr  io.Writer
}

// Unit is what the program of a unit receives: its address and state key,
// and under the unit's lock its work directory; Workdir and State are left
// out when empty.
type Unit struct {
	Address  string
	StateKey string
	Workdir  string
	State    any
	StateDir string // tool/, for a plugin with state: tool
}

// Result is what a program left: its exit code and what it wrote back.
type Result struct {
	ExitCode  int            // 128 + the signal for a program that a signal ended, as in a shell
	Outputs   map[string]any // each declared output that the program gives, null when it wrote none
	OutputErr error          // what made the outputs wrong; Outputs holds what could be read
	RawOutput []byte         // OPSTRUSS_OUTPUT as written, at most MaxOutputs
	Changed   bool
	Message   string
	Sensitive []string
	Plan      []byte // OPSTRUSS_PLAN as written
	Stdout    string // the last Tail bytes of each stream
	Stderr    string
	Read      map[string][]byte // each stream that an output reads, at most MaxOutputs, which the caller logs once it knows whether that output is sensitive
	Warnings  []string
}

// shells run a script by shell:. sh and bash stop at the first command
// that fails, as GitHub Actions runs them; python, node, and pwsh run it
// as written.
var shells = map[string][]string{
	"":       {"sh", "-e", "-c"},
	"sh":     {"sh", "-e", "-c"},
	"bash":   {"bash", "--noprofile", "--norc", "-eo", "pipefail", "-c"},
	"python": {"python3", "-c"},
	"node":   {"node", "-e"},
	"pwsh":   {"pwsh", "-NoProfile", "-NonInteractive", "-Command"},
}

// argv is the program of cmd with args after its own arguments: for a
// script of sh or bash, they are its "$@".
func argv(cmd model.Command, shell string, args []string) ([]string, error) {
	if cmd.Script == "" {
		if len(cmd.Argv) == 0 {
			return nil, errors.New("there is no program to run")
		}
		return append(slices.Clone(cmd.Argv), args...), nil
	}
	s, ok := shells[shell]
	if !ok {
		return nil, fmt.Errorf("unknown shell %s", shell)
	}
	out := append(slices.Clone(s), cmd.Script)
	if len(args) > 0 && (shell == "" || shell == "sh" || shell == "bash") {
		out = append(out, "opstruss") // $0
	}
	return append(out, args...), nil
}

// LookPath finds a program: a path as written, relative to the working
// directory, as a shell does, or a name in path, the PATH of a call. A
// directory in PATH that is not absolute is skipped, as os/exec refuses
// programs found through one.
func LookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not in PATH", name)
}

// MissingInterpreter says why the program at path cannot start when the
// interpreter of its #! line is not there, or "": exec then fails as if the
// program itself were missing. search is the PATH that #!/usr/bin/env
// looks in.
func MissingInterpreter(path, search string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	line, _, _ := strings.Cut(string(head[:n]), "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if !strings.HasPrefix(line, "#!") || len(fields) == 0 {
		return ""
	}
	if _, err := os.Stat(fields[0]); err != nil {
		return "its interpreter " + fields[0] + " does not exist (from its #! line)"
	}
	args := fields[1:]
	if filepath.Base(fields[0]) != "env" || len(args) == 0 {
		return ""
	}
	if args[0] == "-S" && len(args) > 1 {
		args = args[1:]
	}
	if strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=") {
		return ""
	}
	if _, err := LookPath(args[0], search); err != nil {
		return "its interpreter " + args[0] + " is not in PATH (from its #! line)"
	}
	return ""
}

// startError says why cmd cannot start, in place of the fork/exec error of
// Go, and names the interpreter of a #! line that is not there.
func startError(cmd *exec.Cmd, err error) error {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}
	name := filepath.Base(cmd.Path)
	if errors.Is(pe.Err, fs.ErrNotExist) {
		search := os.Getenv("PATH")
		for _, kv := range cmd.Env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				search = v
			}
		}
		if why := MissingInterpreter(cmd.Path, search); why != "" {
			return fmt.Errorf("%s cannot start: %s", name, why)
		}
	}
	return fmt.Errorf("%s cannot start: %w", name, pe.Err)
}

// stream reads one output stream of a program: into the live log and a
// tail, or, when an output reads it, into a buffer of at most MaxOutputs
// and one byte more, which tells that it was too long.
type stream struct {
	r    *os.File
	live io.Writer
	mu   *sync.Mutex
	data bool
	buf  []byte
	done chan struct{}
}

func (s *stream) copy() {
	defer close(s.done)
	b := make([]byte, 32<<10)
	for {
		n, err := s.r.Read(b)
		if n > 0 {
			s.write(b[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *stream) write(p []byte) {
	if s.data {
		if room := MaxOutputs + 1 - len(s.buf); room > 0 {
			s.buf = append(s.buf, p[:min(len(p), room)]...)
		}
		return
	}
	if s.live != nil {
		s.mu.Lock()
		_, _ = s.live.Write(p)
		s.mu.Unlock()
	}
	s.buf = append(s.buf, p...)
	if len(s.buf) > 2*Tail {
		s.buf = s.buf[:copy(s.buf, s.buf[len(s.buf)-Tail:])]
	}
}

func (s *stream) tail() string {
	b := s.buf
	if len(b) > Tail {
		b = b[len(b)-Tail:]
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// Exec runs one program under the contract, and returns once it and every
// process it left in its process group have ended. An error means that
// the program did not start; a program that fails gives its exit code.
// When ctx ends, the program is stopped with c.Stop.
func (r *Run) Exec(ctx context.Context, c *Call) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(r.Temp, ".calls", strconv.FormatInt(r.calls.Add(1), 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	env, err := r.environment(c, inDir(dir), true)
	if err != nil {
		return nil, err
	}
	path, args, cwd, err := program(c, env)
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: args, Dir: cwd, ExtraFiles: r.programFiles(c)}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}

	// The program gets pipes of its own rather than ones that os/exec
	// copies, so the call sees it end even while a process it left holds
	// them.
	var mu sync.Mutex
	streams := map[string]*stream{}
	var ours, theirs []*os.File
	pipe := func(name string, live io.Writer) (*os.File, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		ours, theirs = append(ours, pr), append(theirs, pw)
		s := &stream{r: pr, live: live, mu: &mu, done: make(chan struct{})}
		for _, o := range c.Outputs {
			s.data = s.data || o.From == name
		}
		streams[name] = s
		return pw, nil
	}
	closeAll := func(files []*os.File) {
		for _, f := range files {
			_ = f.Close()
		}
	}
	stdout, err := pipe("stdout", c.Stdout)
	if err == nil {
		cmd.Stdout = stdout
		var stderr *os.File
		stderr, err = pipe("stderr", c.Stderr)
		cmd.Stderr = stderr
	}
	var stdin *os.File
	if err == nil && c.Stdin != nil {
		var pr *os.File
		if pr, stdin, err = os.Pipe(); err == nil {
			cmd.Stdin = pr
			ours, theirs = append(ours, stdin), append(theirs, pr)
		}
	}
	if err != nil {
		closeAll(append(ours, theirs...))
		return nil, err
	}
	g, err := proc.Start(cmd)
	closeAll(theirs)
	if err != nil {
		closeAll(ours)
		return nil, startError(cmd, err)
	}
	failpoint.Hit("program-started")
	for _, s := range streams {
		go s.copy()
	}
	if stdin != nil {
		go func() {
			_, _ = stdin.Write(c.Stdin)
			_ = stdin.Close()
		}()
	}

	res := &Result{}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop := c.Stop
	if stop.Signal == 0 {
		stop = DefaultStop
	}
	// A second Ctrl-C kills at once what the first one stops.
	kill := proc.Kill(ctx)
	select {
	case <-exited:
	case <-ctx.Done():
		if err := g.Stop(stop.Signal, stop.Grace, kill); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		<-exited
	}
	// What the program left in its group stops too, so nothing of it runs
	// once the call returns, as systemd stops the control group of a unit.
	if !g.Empty() {
		res.Warnings = append(res.Warnings, "stopped the processes that the program left in its process group")
		if err := g.Stop(stop.Signal, stop.Grace, kill); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if !drain(streams, PipeWait) {
		res.Warnings = append(res.Warnings, "a process outside the program's process group still holds its output; what it writes later is lost")
	}

	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	res.ExitCode = ws.ExitStatus()
	if ws.Signaled() {
		res.ExitCode = 128 + int(ws.Signal())
	}
	res.Stdout, res.Stderr = streams["stdout"].tail(), streams["stderr"].tail()
	for name, s := range streams {
		if s.data {
			if res.Read == nil {
				res.Read = map[string][]byte{}
			}
			res.Read[name] = s.buf[:min(len(s.buf), MaxOutputs)]
		}
	}
	r.collect(c, dir, res, streams)
	return res, nil
}

// Attach runs one program under the contract on the terminal of the
// caller, with stdin, stdout, and stderr as they are, for a program that
// an operator works in: it stays in the caller's process group, which the
// terminal reaches, and none of its output is read. It returns the
// program's exit code once the program ended; until then nothing stops it,
// since the terminal reaches it.
func (r *Run) Attach(c *Call, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	dir := filepath.Join(r.Temp, ".calls", strconv.FormatInt(r.calls.Add(1), 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	env, err := r.environment(c, inDir(dir), true)
	if err != nil {
		return 0, err
	}
	path, args, cwd, err := program(c, env)
	if err != nil {
		return 0, err
	}
	cmd := &exec.Cmd{Path: path, Args: args, Dir: cwd, Stdin: stdin, Stdout: stdout, Stderr: stderr,
		ExtraFiles: r.programFiles(c)}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	var exit *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exit) {
		return 0, err
	}
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ws.ExitStatus(), nil
}

// Program is a program prepared to live outside a run, such as a service
// with its ready.run and stop.run: what it runs, where, and with what
// environment. Files are the files of its env: values, by path, which are
// written before it starts.
type Program struct {
	SourceFiles    func() []*os.File
	RetainSources  func() func()
	ReleaseSources func()

	Path  string
	Args  []string
	Dir   string
	Env   []string
	Files map[string][]byte
	Stdin []byte
}

// Prepare prepares c as a program that lives outside the run, with the
// environment of such a program: no facts or files of the run, and the
// files of env: values in dir, which outlive the run.
func (r *Run) Prepare(c *Call, dir string) (*Program, error) {
	files := map[string][]byte{}
	env, err := r.environment(c, func(name string, data []byte) (string, error) {
		p := filepath.Join(dir, name)
		files[p] = data
		return p, nil
	}, false)
	if err != nil {
		return nil, err
	}
	path, args, cwd, err := program(c, env)
	if err != nil {
		return nil, err
	}
	p := &Program{SourceFiles: r.SourceFiles, RetainSources: r.RetainSources, Path: path, Args: args, Dir: cwd, Files: files, Stdin: c.Stdin}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		p.Env = append(p.Env, k+"="+env[k])
	}
	return p, nil
}

// Equal reports whether p and q run the same: the same program, arguments,
// directory, environment, input, and files of env: values. A variable that
// is not there differs from one that is empty.
func (p *Program) Equal(q *Program) bool {
	return p != nil && q != nil && p.Path == q.Path && slices.Equal(p.Args, q.Args) && p.Dir == q.Dir &&
		slices.Equal(p.Env, q.Env) && bytes.Equal(p.Stdin, q.Stdin) && maps.EqualFunc(p.Files, q.Files, bytes.Equal)
}

// inDir writes the files of a call in dir, 0600; they go with it after
// the call.
func inDir(dir string) func(string, []byte) (string, error) {
	return func(name string, data []byte) (string, error) {
		p := filepath.Join(dir, name)
		return p, os.WriteFile(p, data, 0o600)
	}
}

// program finds what c runs in env: the path of its program, its argv,
// and its working directory, which must exist.
func program(c *Call, env map[string]string) (path string, args []string, cwd string, err error) {
	cwd = resolve(c.Base, c.Dir)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return "", nil, "", fmt.Errorf("the directory %s does not exist", cwd)
	}
	if args, err = argv(c.Command, c.Shell, c.Args); err != nil {
		return "", nil, "", err
	}
	if c.PluginDir != "" && c.Command.Script == "" && strings.Contains(args[0], "/") {
		args[0] = resolve(c.PluginDir, args[0])
	}
	if path, err = LookPath(args[0], env["PATH"]); err != nil {
		if c.Command.Script == "" && slices.Contains(shellBuiltins, args[0]) {
			err = fmt.Errorf("%s is a shell builtin, not a program; write run: as a string to run it through the shell", args[0])
		}
		return "", nil, "", err
	}
	return path, args, cwd, nil
}

// shellBuiltins are the special built-ins of POSIX sh and the builtins of
// shells that a program list names by mistake; one that is also a
// program, as cd is on macOS, runs as that program.
var shellBuiltins = []string{".", ":", "break", "continue", "eval", "exec", "exit", "export", "readonly", "return",
	"set", "shift", "times", "trap", "unset", "alias", "unalias", "cd", "source", "read", "wait", "umask", "ulimit",
	"local", "declare", "typeset", "pushd", "popd"}

// drain waits up to d for the streams to end, then closes them; it reports
// whether they ended by themselves.
func drain(streams map[string]*stream, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	ended := true
	for _, s := range streams {
		select {
		case <-s.done:
		case <-timer.C:
			ended = false
			for _, s := range streams {
				_ = s.r.Close()
			}
		}
		<-s.done
	}
	for _, s := range streams {
		_ = s.r.Close()
	}
	return ended
}

// resolve reads p relative to base, as paths in a file are.
func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func (r *Run) programFiles(c *Call) []*os.File {
	files := []*os.File{r.tracker.File()}
	if r.SourceFiles != nil {
		files = append(files, r.SourceFiles()...)
	}
	return append(files, c.LeaseFiles...)
}

// Close releases a prepared watch's initial source ownership.
func (p *Program) Close() {
	if p != nil && p.ReleaseSources != nil {
		p.ReleaseSources()
	}
}

func (p *Program) LeaseFiles() []*os.File {
	if p.SourceFiles != nil {
		return p.SourceFiles()
	}
	return nil
}
