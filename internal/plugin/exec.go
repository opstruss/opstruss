package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func argv(cmd model.Command, shell string) ([]string, error) {
	if cmd.Script == "" {
		if len(cmd.Argv) == 0 {
			return nil, errors.New("there is no program to run")
		}
		return cmd.Argv, nil
	}
	s, ok := shells[shell]
	if !ok {
		return nil, fmt.Errorf("unknown shell %s", shell)
	}
	return append(slices.Clone(s), cmd.Script), nil
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
	env, err := r.environment(c, dir)
	if err != nil {
		return nil, err
	}
	cwd := resolve(c.Base, c.Dir)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the directory %s does not exist", cwd)
	}
	args, err := argv(c.Command, c.Shell)
	if err != nil {
		return nil, err
	}
	path, err := LookPath(args[0], env["PATH"])
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: args, Dir: cwd, ExtraFiles: []*os.File{r.tracker.File()}}
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
		return nil, err
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
	select {
	case <-exited:
	case <-ctx.Done():
		if err := g.Stop(stop.Signal, stop.Grace); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		<-exited
	}
	// What the program left in its group stops too, so nothing of it runs
	// once the call returns, as systemd stops the control group of a unit.
	if !g.Empty() {
		res.Warnings = append(res.Warnings, "stopped the processes that the program left in its process group")
		if err := g.Stop(stop.Signal, stop.Grace); err != nil {
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
	r.collect(c, dir, res, streams)
	return res, nil
}

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
