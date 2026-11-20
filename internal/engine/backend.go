package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
)

// The program of a plugin backend: how long each answer may take, how long
// it has to end once its stdin closes, and how long it may sit unused
// before it stops, to start again at the next operation.
const (
	backendWait  = 5 * time.Minute
	backendGrace = 10 * time.Second
)

var backendIdle = time.Minute // shorter in tests

// backends are what one command, with the global scope of its run, keeps
// state in: the local backend, and the program of each plugin backend by
// what it starts from, which the scopes that resolve alike share.
type backends struct {
	mu     sync.Mutex
	local  *state.Local
	plugin map[string]*pluginBackend
}

func (bs *backends) close() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for _, b := range bs.plugin {
		b.close()
	}
}

// backendError is a plugin backend that does not start, which doctor
// reports.
type backendError struct {
	scope, name string
	err         error
}

func (e *backendError) Error() string { return e.err.Error() }
func (e *backendError) Unwrap() error { return e.err }

// backendOf is the backend that p, read for the scope of e, keeps its
// state in: the local one, or the program of its plugin backend with the
// inputs that with: gives, which are read before the state opens.
func (e *engine) backendOf(p *model.Project) (state.Backend, *pluginBackend, error) {
	be, pb, err := e.startBackend(p)
	if err != nil && p.Backend != nil {
		return nil, nil, &backendError{scope: p.StackName, name: p.Backend.Uses, err: err}
	}
	return be, pb, err
}

func (e *engine) startBackend(p *model.Project) (state.Backend, *pluginBackend, error) {
	bs := e.backends
	b := p.Backend
	if b == nil || b.Local {
		bs.mu.Lock()
		defer bs.mu.Unlock()
		if bs.local == nil {
			bs.local = state.NewLocal(filepath.Join(e.root, "state"))
		}
		return bs.local, nil, nil
	}
	pkg := b.Package
	if pkg == nil || pkg.Plugin == nil || pkg.Plugin.Backend == nil {
		return nil, nil, fmt.Errorf("backend %s is not a plugin backend", b.Uses)
	}
	pl := pkg.Plugin
	given := map[string]any{}
	if d := pkg.Decl; d != nil {
		maps.Copy(given, d.With)
	}
	maps.Copy(given, b.With)
	vars, err := e.backendVars()
	if err != nil {
		return nil, nil, e.redact.Err(fmt.Errorf("backend: with: %w", err))
	}
	s := &expr.Scope{Dir: p.DirOf("backend"), Vars: vars, Marks: map[string]any{"inputs": e.inputMarks, "locals": e.localMarks}}
	v, marks, err := expr.EvalMarked(given, s)
	if err != nil {
		return nil, nil, e.redact.Err(fmt.Errorf("backend: with: %w", err))
	}
	values := v.(map[string]any)
	m, _ := marks.(map[string]any)
	for k, val := range values {
		switch {
		case pl.Inputs[k] != nil && pl.Inputs[k].Sensitive:
			e.known("input "+k+" of the backend", val)
		case m[k] != nil:
			e.hide(val, m[k])
		}
	}
	inputs, err := conform(pl.Inputs, values, given, s)
	if err != nil {
		return nil, nil, e.redact.Err(fmt.Errorf("backend: with: %w", err))
	}
	allowed := slices.Clone(b.Pass)
	if d := pkg.Decl; d != nil {
		allowed = append(allowed, d.Pass...)
	}
	var pass []string
	for _, kv := range e.opts.Environ {
		k, _, _ := strings.Cut(kv, "=")
		if pl.Asks(k) && !model.ReservedEnv(k) && !slices.Contains(pass, k) &&
			slices.ContainsFunc(allowed, func(p string) bool { ok, _ := path.Match(p, k); return ok }) {
			pass = append(pass, k)
		}
	}
	slices.Sort(pass)
	search := e.pluginPath(pkg)
	for _, name := range pl.Requires {
		if _, err := plugin.LookPath(name, search); err != nil {
			return nil, nil, fmt.Errorf("backend %s requires %s, which is not in PATH", b.Uses, name)
		}
	}
	key := expr.JSON(map[string]any{"dir": pkg.Dir, "inputs": inputs, "pass": pass})
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if pb := bs.plugin[key]; pb != nil {
		return pb, pb, nil
	}
	// The call has no env:, so its program has no files to write.
	c := &plugin.Call{Command: *pl.Backend.Run, Base: p.DirOf("backend"), PluginDir: pkg.Dir, BinDirs: pkg.BinDirs,
		Pass: pass, Inputs: inputs, Backend: true}
	prog, err := e.outside().Prepare(c, "")
	if err != nil {
		return nil, nil, fmt.Errorf("backend %s: %w", b.Uses, err)
	}
	pb := &pluginBackend{name: b.Uses, prog: prog, log: e.backendLine, dead: e.holderDead, took: e.tookLock}
	pb.file = state.BackendFile{Uses: b.Uses, With: given, Pass: allowed}
	if id := pkg.Source; id != nil {
		pb.file.Uses, pb.file.Commit, pb.file.Hash = id.Key(), id.Commit, id.Hash
	} else if rel, err := filepath.Rel(p.Dir, pkg.Dir); err == nil {
		if pb.file.Uses = filepath.ToSlash(rel); !strings.HasPrefix(pb.file.Uses, "../") {
			pb.file.Uses = "./" + pb.file.Uses
		}
	}
	pb.source = identitySource(pb.file.Uses)
	if err := pb.begin(); err != nil {
		return nil, nil, e.redact.Err(fmt.Errorf("backend %s: %w", b.Uses, err))
	}
	if bs.plugin == nil {
		bs.plugin = map[string]*pluginBackend{}
	}
	bs.plugin[key] = pb
	return pb, pb, nil
}

// backendVars are what with: of a backend reads before the state opens,
// and so outside a run: the inputs, the secrets from sops and env, and the
// locals, which read those secrets too.
func (e *engine) backendVars() (map[string]any, error) {
	secrets := map[string]any{}
	var locals map[string]any
	for name, src := range e.p.Secrets {
		if src.Sops == "" && src.EnvName == "" {
			continue
		}
		secrets[name] = expr.Later(func() (any, error) {
			v, err := e.secretFrom(name, locals)
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", name, err)
			}
			e.known("secret "+name, v)
			return v, nil
		})
	}
	locals, err := e.p.EvalLocals(e.inputs, nil, secrets)
	if err != nil {
		return nil, err
	}
	return map[string]any{"inputs": e.inputs, "locals": locals, "secrets": secrets}, nil
}

// backendLine reports what the program of a plugin backend wrote to its
// stderr.
func (e *engine) backendLine(name, line string) {
	e.notice(Event{Type: "diagnostic", Level: "warn", Message: "backend " + name + ": " + line})
}

// notice sends an event from what may run beside the run, such as a
// plugin backend: into the log of the run, or to the caller before the
// run starts and in a command without one.
func (e *engine) notice(ev Event) {
	e.mu.Lock()
	rec := e.rec
	e.mu.Unlock()
	if rec != nil {
		e.emit(ev)
		return
	}
	if e.opts.Emit != nil {
		ev.Message = e.redact.Log(ev.Message)
		ev.Time = record.Stamp(time.Now())
		e.opts.Emit(ev)
	}
}

// holderDead reports whether the holder of a lock is proven dead: it ran
// on this host, its pid no longer runs the process that started then, and
// no program of its run lives on, by its environment or by the lock of the
// run that each one inherits.
func (e *engine) holderDead(h state.Holder) bool {
	if h.Host == "" || h.Host != e.host || h.PID <= 0 || h.Start == "" {
		return false
	}
	if start, err := proc.StartTime(h.PID); err == nil && start == h.Start {
		return false
	}
	if h.Run == "" {
		return true
	}
	ps, held, err := plugin.Marker(h.Run, e.tempOf(h.Run)).Alive()
	return err == nil && len(ps) == 0 && !held
}

// tookLock says that a lock whose holder died on this host was taken.
func (e *engine) tookLock(key string, h state.Holder) {
	e.notice(Event{Type: "diagnostic", Level: "warn", Message: fmt.Sprintf("took over the lock %s of %s, which no longer runs", key, h)})
}

// pluginBackend is the program of a plugin backend. It starts at the first
// operation and stops once the command is done with it, or after a minute
// without operations, to start again at the next; one that stopped
// answering stays broken, and its operations fail.
type pluginBackend struct {
	name   string // the backend as the project names it
	source string // the plugin without its version, which with the location is the identity of the backend
	file   state.BackendFile
	// prog is the program, which gets no lease of the sources: it reads its
	// files as it starts, and ends with the stdin that OpsTruss holds.
	prog *plugin.Program
	log  func(name, line string)
	dead func(state.Holder) bool
	took func(key string, h state.Holder)

	mu       sync.Mutex
	location string
	cur      *backendProcess
	failed   error
	busy     int
	idle     *time.Timer
}

type backendProcess struct {
	remote *state.Remote
	stdin  *os.File
	stdout *os.File
	stop   context.CancelFunc // stops the process group
	done   chan struct{}      // closed once the program and its group ended
	state  *os.ProcessState

	mu   sync.Mutex
	last string // the last line of stderr
}

// begin starts the program, which gives the location of the state.
func (b *pluginBackend) begin() error {
	_, err := b.acquire()
	if err == nil {
		b.done()
	}
	return err
}

// acquire returns the program for one operation, started when it is not
// running; done ends the operation.
func (b *pluginBackend) acquire() (*state.Remote, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed != nil {
		return nil, b.failed
	}
	if b.idle != nil {
		b.idle.Stop()
	}
	if b.cur == nil {
		p, err := b.start()
		if err != nil {
			b.failed = err
			return nil, err
		}
		if b.location != "" && p.remote.Location != b.location {
			p.end()
			b.failed = fmt.Errorf("the backend started again at %s, not %s", p.remote.Location, b.location)
			return nil, b.failed
		}
		b.cur, b.location = p, p.remote.Location
	}
	b.busy++
	return b.cur.remote, nil
}

func (b *pluginBackend) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.busy--
	if b.busy > 0 || b.cur == nil {
		return
	}
	// A new timer each time, for the program that runs now: one reset
	// would stop only the program it was made for, which may have stopped.
	cur := b.cur
	if b.idle != nil {
		b.idle.Stop()
	}
	b.idle = time.AfterFunc(backendIdle, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.busy == 0 && b.cur == cur {
			b.cur = nil
			go cur.end()
		}
	})
}

// close stops the program, once the command is done with it.
func (b *pluginBackend) close() {
	b.mu.Lock()
	cur := b.cur
	b.cur = nil
	if b.idle != nil {
		b.idle.Stop()
	}
	b.mu.Unlock()
	if cur != nil {
		cur.end()
	}
}

// broke keeps the error of the connection r that broke: the program is
// gone or stopped answering, and the operations after it fail with it.
func (b *pluginBackend) broke(r *state.Remote) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur != nil && b.cur.remote == r {
		b.failed = r.Broken()
		cur := b.cur
		b.cur = nil
		go cur.end()
	}
}

// start runs the program and opens it with initialize.
func (b *pluginBackend) start() (*backendProcess, error) {
	prog := b.prog
	var ours, theirs []*os.File
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			_ = f.Close()
		}
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll([]*os.File{inR, inW})
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll([]*os.File{inR, inW, outR, outW})
		return nil, err
	}
	ours, theirs = []*os.File{inW, outR, errR}, []*os.File{inR, outW, errW}
	cmd := &exec.Cmd{Path: prog.Path, Args: prog.Args, Dir: prog.Dir, Env: prog.Env, Stdin: inR, Stdout: outW, Stderr: errW}
	g, err := proc.Start(cmd)
	closeAll(theirs)
	if err != nil {
		closeAll(ours)
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	p := &backendProcess{stdin: inW, stdout: outR, stop: stop, done: make(chan struct{})}
	go func() {
		_, _ = proc.Wait(ctx, cmd, g, syscall.SIGTERM, backendGrace)
		p.state = cmd.ProcessState
		close(p.done)
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(errR)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.last = line
			p.mu.Unlock()
			b.log(b.name, line)
		}
		_, _ = io.Copy(io.Discard, errR)
		_ = errR.Close()
	}()
	p.remote = &state.Remote{In: inW, Out: outR, Timeout: backendWait, Stop: stop, Ended: func() error {
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			return nil
		}
		<-stderrDone
		return p.exit()
	}}
	if err := p.remote.Open(); err != nil {
		p.end()
		return nil, err
	}
	return p, nil
}

// exit says how the program ended, with the last line it wrote to stderr.
func (p *backendProcess) exit() error {
	p.mu.Lock()
	last := p.last
	p.mu.Unlock()
	what := "it ended"
	if p.state != nil {
		what = fmt.Sprintf("it exited with %d", proc.ExitCode(p.state))
	}
	if last != "" {
		what += ": " + last
	}
	return errors.New(what)
}

// end closes the program's stdin, waits for it to end, and stops its
// process group when it does not in time.
func (p *backendProcess) end() {
	p.remote.Close()
	_ = p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(backendGrace):
		p.stop()
		<-p.done
	}
	p.stop()
	_ = p.stdout.Close()
}

// The operations of the contract, each on the running program.

func (b *pluginBackend) Get(key string) (data []byte, etag string, err error) {
	err = b.do(func(r *state.Remote) error { data, etag, err = r.Get(key); return err })
	return data, etag, err
}

func (b *pluginBackend) Put(key string, data []byte, ifMatch string) (etag string, err error) {
	err = b.do(func(r *state.Remote) error { etag, err = r.Put(key, data, ifMatch); return err })
	return etag, err
}

func (b *pluginBackend) Delete(key string, ifMatch string) error {
	return b.do(func(r *state.Remote) error { return r.Delete(key, ifMatch) })
}

func (b *pluginBackend) List(prefix string) (keys []string, err error) {
	err = b.do(func(r *state.Remote) error { keys, err = r.List(prefix); return err })
	return keys, err
}

// Lock takes the lock, and takes over a lock whose holder is proven dead
// on this host, with the holder's token: a lock of a plugin backend does
// not go away when its holder dies.
func (b *pluginBackend) Lock(key string, h state.Holder) (string, error) {
	token, err := b.tryLock(key, h)
	var le *state.LockedError
	if errors.As(err, &le) && b.dead != nil && b.dead(le.Holder) {
		if uerr := b.Unlock(key, le.Token); uerr == nil {
			b.took(key, le.Holder)
			token, err = b.tryLock(key, h)
		}
	}
	return token, err
}

// tryLock takes the lock without taking over a dead holder's.
func (b *pluginBackend) tryLock(key string, h state.Holder) (token string, err error) {
	err = b.do(func(r *state.Remote) error { token, err = r.Lock(key, h); return err })
	return token, err
}

func (b *pluginBackend) Unlock(key, token string) error {
	return b.do(func(r *state.Remote) error { return r.Unlock(key, token) })
}

func (b *pluginBackend) do(op func(*state.Remote) error) error {
	r, err := b.acquire()
	if err != nil {
		return err
	}
	err = op(r)
	if r.Broken() != nil {
		b.broke(r)
	}
	b.done()
	return err
}

// openStore opens the state of the scope that p is read for in its
// backend, and checks that this checkout kept the scope there before.
func (e *engine) openStore(p *model.Project) (*state.Store, error) {
	be, pb, err := e.backendOf(p)
	if err != nil {
		return nil, err
	}
	s, err := state.Open(be, p.StackName, p.Name)
	if err != nil {
		return nil, err
	}
	s.Key = e.dataKey
	s.Known = func(_, _ string, v any) bool { return e.redact.Contains(v) }
	s.Opened = func(v any) { e.redact.Add(v) }
	if err := e.checkOrigin(p.StackName, pb, s); err != nil {
		if !e.opts.report {
			return nil, err
		}
		e.originErr = err
	}
	return s, nil
}

// backendID names the backend be by what makes two backends the same, as
// messages name it too: the local one, or the location of a plugin backend
// and its plugin without the version.
func backendID(be state.Backend) string {
	if pb, ok := be.(*pluginBackend); ok {
		return pb.location + " through " + pb.source
	}
	return "the local backend"
}

// identitySource is the plugin of a backend without its version: with the
// location, what makes two backends the same.
func identitySource(uses string) string {
	if id, err := sources.Parse(uses); err == nil {
		return id.Address()
	}
	return path.Clean(filepath.ToSlash(uses))
}

// scopeText and scopeFlag name a scope in messages.
func scopeText(scope string) string {
	if scope == model.GlobalScope {
		return "the global scope"
	}
	return "stack " + scope
}

func scopeFlag(scope string) string {
	if scope == model.GlobalScope {
		return "--global"
	}
	return "--stack " + scope
}

// checkOrigin compares the backend that the project names for scope with
// the one that backend.json says this checkout kept the scope in: no file
// is the local backend. A plugin backend needs the _meta of the scope, and
// a first command of a new checkout that finds it writes backend.json.
func (e *engine) checkOrigin(scope string, pb *pluginBackend, s *state.Store) error {
	root := filepath.Join(e.root, "state")
	was, err := state.ReadBackendFile(root, scope)
	if err != nil {
		return err
	}
	what, flag := scopeText(scope), scopeFlag(scope)
	if pb == nil {
		if was == nil {
			return nil
		}
		return fmt.Errorf("this checkout keeps the state of %s in %s through %s, and the project now keeps it in the local backend; move it with opstruss state migrate %s",
			what, was.Location, identitySource(was.Uses), flag)
	}
	if was != nil && (identitySource(was.Uses) != pb.source || was.Location != pb.location) {
		return fmt.Errorf("this checkout keeps the state of %s in %s through %s, and the project now names %s through %s; move it with opstruss state migrate %s",
			what, was.Location, identitySource(was.Uses), pb.location, pb.source, flag)
	}
	if was == nil {
		addrs, err := (&state.Store{Backend: state.NewLocal(root), Stack: scope}).Inventory()
		if err != nil {
			return err
		}
		if len(addrs) > 0 {
			n := fmt.Sprintf("%d records", len(addrs))
			if len(addrs) == 1 {
				n = "a record"
			}
			return fmt.Errorf("%s has %s in the local backend of this checkout, and the project now keeps its state in %s; move it there with opstruss state migrate %s",
				what, n, pb.location, flag)
		}
	}
	if !s.HasMeta() {
		init := "opstruss init --stack " + scope
		if scope == model.GlobalScope {
			init = "opstruss init"
		}
		return &noMetaError{fmt.Sprintf("%s has no state in %s; start it there with %s, or move its state there with opstruss state migrate %s",
			what, pb.location, init, flag)}
	}
	return e.keepOrigin(scope, pb, was)
}

// keepOrigin writes backend.json of scope for pb, unless it says so.
func (e *engine) keepOrigin(scope string, pb *pluginBackend, was *state.BackendFile) error {
	now := pb.file
	now.Location, now.Version = pb.location, state.BackendFileVersion
	if was != nil && expr.JSON(was) == expr.JSON(&now) {
		return nil
	}
	return state.WriteBackendFile(filepath.Join(e.root, "state"), scope, &now)
}
