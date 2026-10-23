package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/secret"
)

// ServiceID is the variable that marks every process of a program the
// daemon guards, which each program passes on to the programs it starts.
const ServiceID = "OPSTRUSS_SERVICE_ID"

// Restarts back off as Docker's do: the first delay doubles up to the
// longest, and starts over once a service ran for resetAfter.
var (
	firstDelay = 100 * time.Millisecond
	maxDelay   = time.Minute
	resetAfter = 10 * time.Second
	// sweepEvery is how often what is left of programs that ended is
	// looked at again.
	sweepEvery = 2 * time.Second
)

// gate is what a program starts as: /bin/sh that waits for the daemon to
// write in the pipe on descriptor 4, once the program's pid is recorded,
// and then becomes the program with the same pid, as runc start releases
// what runc create made. A daemon that dies first closes the pipe, and the
// program never runs.
const gate = `read go <&4 || exit 1; exec 4<&-; exec "$@"`

// Supervisor keeps the services of one stack: it starts each program
// through the gate, records it before it runs, restarts it by its
// restart:, and stops it with stop.run, its signal, and a kill after its
// grace.
type Supervisor struct {
	root, stack string
	reg         *registry
	say         func(record.Event) // the daemon's log
	watch       func(record.Event) // the terminal of a daemon in the foreground, or nil
	kill        <-chan struct{}    // closed when what stops the services is to kill them at once

	recovered chan struct{} // closed once what a daemon that ended left is stopped
	done      chan struct{} // closed once the supervisor stops

	mu        sync.Mutex
	services  map[string]*service
	listeners map[string]map[int]func(stream, line string) // of the lines of services, by address
	nextID    int
	unclear   map[string]map[string]string // by address and id: what is left that cannot be proven the service's
	closed    bool
}

func newSupervisor(root, stack string, reg *registry, say func(record.Event)) *Supervisor {
	return &Supervisor{root: root, stack: stack, reg: reg, say: say, recovered: make(chan struct{}),
		done: make(chan struct{}), services: map[string]*service{}, unclear: map[string]map[string]string{}}
}

// event reports on the service at addr in the daemon's log, and at the
// terminal of a daemon in the foreground.
func (sup *Supervisor) event(level, addr, format string, args ...any) {
	ev := record.Event{Type: "service", Level: level, Address: addr, Message: fmt.Sprintf(format, args...)}
	sup.say(ev)
	if sup.watch != nil {
		sup.watch(ev)
	}
}

// recover stops what a daemon that ended left of its programs, before any
// starts again, and then looks again now and then at what is left.
func (sup *Supervisor) recover() {
	for _, p := range sup.reg.processes() {
		s, err := survey(sup.root, p)
		if err == nil && len(s.stop) > 0 {
			sup.event("warn", p.Address, "stopping what the daemon that ended left: %s", s)
			s.halt(p)
			s, err = survey(sup.root, p)
		}
		if err != nil {
			s.unclear = err.Error()
		}
		sup.settle(p, s)
	}
	close(sup.recovered)
	go sup.sweep()
}

// sweep forgets each process entry of a program that ended once nothing
// of it is left, until the supervisor stops.
func (sup *Supervisor) sweep() {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-sup.done:
			return
		case <-t.C:
		}
		for _, p := range sup.reg.processes() {
			if sup.current(p) {
				continue
			}
			if s, err := survey(sup.root, p); err == nil {
				sup.settle(p, s)
			}
		}
	}
}

// current reports whether p is the program of a service that runs.
func (sup *Supervisor) current(p *Process) bool {
	sup.mu.Lock()
	s := sup.services[p.Address]
	sup.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && s.cur.p.ID == p.ID
}

// settle forgets the entry p once nothing of it is left, or keeps it with
// what is left that cannot be proven its own.
func (sup *Supervisor) settle(p *Process, s leftover) {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if !s.clear() {
		if s.unclear != "" {
			if sup.unclear[p.Address] == nil {
				sup.unclear[p.Address] = map[string]string{}
			}
			sup.unclear[p.Address][p.ID] = s.unclear
		}
		return
	}
	if err := sup.reg.update(func(f *File) { f.Processes = without(f.Processes, p.ID) }); err != nil {
		return
	}
	delete(sup.unclear[p.Address], p.ID)
	if len(sup.unclear[p.Address]) == 0 {
		delete(sup.unclear, p.Address)
	}
	removeLock(sup.root, p)
}

// Start stops the service at s.Address when it runs, starts s, and returns
// once it is ready, or stops it again when it is not ready in time.
func (sup *Supervisor) Start(ctx context.Context, spec *engine.Service) error {
	select {
	case <-sup.recovered:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	addr := spec.Address
	sup.mu.Lock()
	if sup.closed {
		sup.mu.Unlock()
		return errors.New("the daemon of the stack stops")
	}
	if u := sup.unclear[addr]; len(u) > 0 {
		sup.mu.Unlock()
		return fmt.Errorf("%s; once you are sure it is not the service's, opstruss unlock %s forgets it",
			strings.Join(sortedValues(u), "; "), sup.short(addr))
	}
	prev := sup.services[addr]
	s := &service{sup: sup, spec: spec, ready: make(chan error, 1), stopped: make(chan struct{}), done: make(chan struct{}),
		status: engine.ServiceStatus{State: "starting"}}
	sup.services[addr] = s
	sup.mu.Unlock()
	if prev != nil {
		prev.stop()
	}
	retain := spec.RetainSources
	if retain == nil {
		retain = spec.Program.RetainSources
	}
	if retain != nil {
		release := retain()
		go func() { defer release(); s.loop() }()
	} else {
		go s.loop()
	}
	var timeout <-chan time.Time
	if spec.Ready.HTTP != "" || spec.Ready.TCP != "" || spec.Ready.Run != nil {
		t := time.NewTimer(spec.Ready.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var err error
	select {
	case err = <-s.ready:
	case <-timeout:
		err = fmt.Errorf("not ready after %s", spec.Ready.Timeout)
		if last := s.lastCheck(); last != nil {
			err = fmt.Errorf("%w: %v", err, last)
		}
	case <-ctx.Done():
		err = context.Cause(ctx)
	}
	if err != nil {
		sup.drop(s)
		return err
	}
	return nil
}

// drop stops s and forgets it, unless another start took its address.
func (sup *Supervisor) drop(s *service) {
	s.stop()
	sup.mu.Lock()
	if sup.services[s.spec.Address] == s {
		delete(sup.services, s.spec.Address)
	}
	sup.mu.Unlock()
}

// Stop stops the service at addr and forgets it; what cannot be proven
// gone of an earlier program of it fails the stop.
func (sup *Supervisor) Stop(_ context.Context, addr string) error {
	sup.mu.Lock()
	s := sup.services[addr]
	sup.mu.Unlock()
	if s != nil {
		sup.drop(s)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if u := sup.unclear[addr]; len(u) > 0 {
		return fmt.Errorf("%s; once you are sure it is not the service's, opstruss unlock %s forgets it",
			strings.Join(sortedValues(u), "; "), sup.short(addr))
	}
	return nil
}

// stopSpec stops the service that spec started and waits until it ended,
// unless another start took its address since.
func (sup *Supervisor) stopSpec(spec *engine.Service) {
	sup.mu.Lock()
	s := sup.services[spec.Address]
	sup.mu.Unlock()
	if s != nil && s.spec == spec {
		sup.drop(s)
	}
}

// specLives reports whether the service that spec started still runs, or
// starts again, and has not given up.
func (sup *Supervisor) specLives(spec *engine.Service) bool {
	sup.mu.Lock()
	s := sup.services[spec.Address]
	sup.mu.Unlock()
	if s == nil || s.spec != spec {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.State != "stopped"
}

// Status says how the service at addr runs.
func (sup *Supervisor) Status(addr string) (engine.ServiceStatus, bool) {
	sup.mu.Lock()
	s := sup.services[addr]
	sup.mu.Unlock()
	if s == nil {
		return engine.ServiceStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, true
}

// Forget forgets the entries of the service at addr that cannot be proven
// its own.
func (sup *Supervisor) Forget(addr string) ([]string, error) {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	u := sup.unclear[addr]
	if len(u) == 0 {
		return nil, nil
	}
	var gone []*Process
	for _, p := range sup.reg.processes() {
		if _, ok := u[p.ID]; ok {
			gone = append(gone, p)
		}
	}
	err := sup.reg.update(func(f *File) {
		for _, p := range gone {
			f.Processes = without(f.Processes, p.ID)
		}
	})
	if err != nil {
		return nil, err
	}
	for _, p := range gone {
		removeLock(sup.root, p)
	}
	delete(sup.unclear, addr)
	return sortedValues(u), nil
}

// stopAll stops every service, as the daemon stops, and the supervisor
// with them.
func (sup *Supervisor) stopAll() {
	<-sup.recovered
	sup.mu.Lock()
	sup.closed = true
	list := make([]*service, 0, len(sup.services))
	for _, s := range sup.services {
		list = append(list, s)
	}
	sup.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Go(s.stop)
	}
	wg.Wait()
	close(sup.done)
}

// live reports whether a service runs, starts, or waits to restart.
func (sup *Supervisor) live() bool {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	for _, s := range sup.services {
		select {
		case <-s.done:
		default:
			return true
		}
	}
	return false
}

func (sup *Supervisor) short(addr string) string { return strings.TrimPrefix(addr, sup.stack+"/") }

// listen hands each line that the service at addr writes to fn, until
// the function it returns is called.
func (sup *Supervisor) listen(addr string, fn func(stream, line string)) func() {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.listeners == nil {
		sup.listeners = map[string]map[int]func(string, string){}
	}
	if sup.listeners[addr] == nil {
		sup.listeners[addr] = map[int]func(string, string){}
	}
	sup.nextID++
	id := sup.nextID
	sup.listeners[addr][id] = fn
	return func() {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		delete(sup.listeners[addr], id)
	}
}

func (sup *Supervisor) listenersOf(addr string) []func(string, string) {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	var out []func(string, string)
	for _, fn := range sup.listeners[addr] {
		out = append(out, fn)
	}
	return out
}

// service is one service and the loop that keeps it.
type service struct {
	sup  *Supervisor
	spec *engine.Service
	log  *record.Log

	ready     chan error // the first time it is ready, or why it never was
	readyOnce sync.Once
	stopped   chan struct{} // closed when it is to stop
	done      chan struct{} // closed when the loop ended

	mu       sync.Mutex
	status   engine.ServiceStatus
	cur      *child
	started  bool // the first program started
	stopping bool
	check    error // why the last ready check failed
}

// child is one program of a service, from the gate to its end.
type child struct {
	p       *Process
	cmd     *exec.Cmd
	lock    *os.File // the daemon's hold of the lock the program inherits, until it ended
	exited  chan struct{}
	code    int
	out     sync.WaitGroup // its output, copied to the log
	outs    []*os.File
	helpers sync.WaitGroup // its ready checks
}

// loop starts the service, and starts it again by its restart: until it
// stops.
func (s *service) loop() {
	defer close(s.done)
	addr := s.spec.Address
	if s.spec.Lines == nil {
		var err error
		if s.log, err = record.OpenLog(filepath.Join(s.sup.root, "logs", s.sup.stack, filepath.FromSlash(s.sup.short(addr))+".jsonl")); err != nil {
			s.end(err)
			return
		}
		defer func() { _ = s.log.Close() }()
	}
	defer s.removeFiles()
	defer func() { s.spec.Program.Close() }()
	delay := firstDelay
	for {
		if s.isStopping() {
			s.end(errors.New("stopped before it was ready"))
			return
		}
		c, err := s.start()
		if err != nil {
			s.sup.event("error", addr, "could not start: %v", err)
			s.end(err)
			return
		}
		began := time.Now()
		c.helpers.Add(1)
		go func() {
			defer c.helpers.Done()
			s.watchReady(c)
		}()
		select {
		case <-c.exited:
		case <-s.stopped:
			s.halt(c)
			<-c.exited
		}
		s.cleanup(c)
		code := c.code
		if s.isStopping() {
			s.sup.event("info", addr, "stopped, exit code %d", code)
			s.end(errors.New("stopped before it was ready"))
			return
		}
		if !restarts(s.spec.Restart, code) {
			s.sup.event(map[bool]string{true: "info", false: "warn"}[code == 0], addr, "exited with %d", code)
			s.end(fmt.Errorf("exited with %d before it was ready", code))
			return
		}
		if time.Since(began) >= resetAfter {
			delay = firstDelay
		}
		s.sup.event("warn", addr, "exited with %d; restarting in %s", code, delay)
		s.set("restarting", 0)
		select {
		case <-time.After(delay):
		case <-s.stopped:
			s.end(errors.New("stopped before it was ready"))
			return
		}
		delay = min(2*delay, maxDelay)
		s.mu.Lock()
		s.status.Restarts++
		s.mu.Unlock()
	}
}

// restarts reports whether a program that exited with code starts again.
func restarts(policy string, code int) bool {
	switch policy {
	case "always":
		return true
	case "no":
		return false
	}
	return code != 0
}

// end marks the service stopped, and err as why it was never ready, when
// it was not.
func (s *service) end(err error) {
	s.set("stopped", 0)
	s.readyOnce.Do(func() { s.ready <- err })
}

func (s *service) set(state string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State, s.status.PID, s.status.Since = state, pid, time.Now().UTC()
}

func (s *service) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

func (s *service) lastCheck() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.check
}

// start starts the program of the service through the gate: in a session
// of its own, with its own lock and OPSTRUSS_SERVICE_ID, recorded in
// <stack>.json before the gate lets it run.
func (s *service) start() (_ *child, err error) {
	if s.started && s.spec.Rebuild != nil {
		p, err := s.spec.Rebuild()
		if err != nil {
			return nil, err
		}
		s.spec.Program.Close()
		s.spec.Program = p
	}
	s.started = true
	prog := s.spec.Program
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.spec.Dir, 0o700); err != nil {
		return nil, err
	}
	for _, p := range []*plugin.Program{prog, s.spec.Ready.Run, s.spec.StopRun} {
		if p == nil {
			continue
		}
		for path, data := range p.Files {
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return nil, err
			}
		}
	}
	lockPath := filepath.Join(s.spec.Dir, id+".lock")
	rel, err := filepath.Rel(s.sup.root, lockPath)
	if err != nil {
		return nil, err
	}
	t, err := proc.Track(lockPath)
	if err != nil {
		return nil, err
	}
	c := &child{exited: make(chan struct{}), lock: t.File()}
	var ours, theirs []*os.File
	closeAll := func(files []*os.File) {
		for _, f := range files {
			_ = f.Close()
		}
	}
	defer func() {
		closeAll(theirs)
		if err != nil {
			closeAll(ours)
			_ = c.lock.Close()
			_ = os.Remove(lockPath)
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			ours, theirs = append(ours, r), append(theirs, w)
		}
		return r, w, err
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ours, theirs = append(ours, gateW), append(theirs, gateR)
	outR, outW, err := pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Path: "/bin/sh", Args: append([]string{"sh", "-c", gate, "opstruss", prog.Path}, prog.Args[1:]...),
		Dir: prog.Dir, Env: append(slices.Clone(prog.Env), ServiceID+"="+id), Stdout: outW, Stderr: errW,
		ExtraFiles: append([]*os.File{c.lock, gateR}, prog.LeaseFiles()...), SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	var stdin *os.File
	if prog.Stdin != nil {
		var r *os.File
		if r, stdin, err = os.Pipe(); err != nil {
			return nil, err
		}
		cmd.Stdin = r
		ours, theirs = append(ours, stdin), append(theirs, r)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	kill := func() {
		_ = proc.Group(pid).Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	}
	startTime, err := proc.StartTime(pid)
	if err != nil {
		kill()
		return nil, err
	}
	c.p = &Process{Address: s.spec.Address, ID: id, PID: pid, Start: startTime, Started: time.Now().UTC().Format(time.RFC3339),
		Lock: filepath.ToSlash(rel), Signal: proc.SignalName(s.spec.Stop.Signal), Grace: s.spec.Stop.Grace.String()}
	if err := s.sup.reg.update(func(f *File) { f.Processes = append(f.Processes, c.p) }); err != nil {
		kill()
		return nil, err
	}
	failpoint.Hit("service-before-go")
	if _, err := gateW.Write([]byte("go\n")); err != nil {
		kill()
		return nil, err
	}
	_ = gateW.Close()
	c.cmd, c.outs = cmd, []*os.File{outR, errR}
	for i, r := range c.outs {
		c.out.Add(1)
		go s.copy(c, r, []string{"stdout", "stderr"}[i])
	}
	if stdin != nil {
		go func() {
			_, _ = stdin.Write(prog.Stdin)
			_ = stdin.Close()
		}()
	}
	go func() {
		_ = cmd.Wait()
		ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
		c.code = ws.ExitStatus()
		if ws.Signaled() {
			c.code = 128 + int(ws.Signal())
		}
		close(c.exited)
	}()
	s.mu.Lock()
	s.cur = c
	s.status.State, s.status.PID, s.status.Since = "starting", pid, time.Now().UTC()
	s.mu.Unlock()
	s.sup.event("info", s.spec.Address, "started, pid %d", pid)
	return c, nil
}

// copy turns one output stream of a program into log events.
func (s *service) copy(c *child, r *os.File, stream string) {
	defer c.out.Done()
	s.copyLines(r, "", stream)
}

// copyLines turns what r gives into log events, a line each, with the
// sensitive values of the run that started the service hidden, also in a
// private key that spans lines; a line longer than secret.MaxLine goes in
// pieces, cut where no value that the run hides is cut in two. A watch
// source, whose lines are events and no log, has none hidden.
func (s *service) copyLines(r io.Reader, task, stream string) {
	var mask *secret.Stream
	if s.spec.Redact != nil {
		mask = s.spec.Redact.Stream()
	}
	lines := secret.NewLines(mask, func(text string) { s.line(task, stream, text) })
	_, _ = io.Copy(lines, r)
	lines.Flush()
}

// line writes one line of output, hidden already, to the log of the
// service; task names stop.run and ready.run as stop and ready.
func (s *service) line(task, stream, text string) {
	if s.spec.Lines != nil {
		if task == "" {
			s.spec.Lines(stream, text)
		}
		return
	}
	for _, fn := range s.sup.listenersOf(s.spec.Address) {
		if task == "" {
			fn(stream, text)
		}
	}
	ev := s.log.Emit(record.Event{Type: "log", Level: "info", Address: s.spec.Address, Task: task, Stream: stream,
		Message: text})
	if s.sup.watch != nil {
		s.sup.watch(ev)
	}
}

// watchReady checks c until it is ready, then marks it running; the first
// time, Start hears of it.
func (s *service) watchReady(c *child) {
	rd := s.spec.Ready
	if rd.HTTP == "" && rd.TCP == "" && rd.Run == nil {
		s.running(c)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c.exited:
		case <-s.stopped:
		}
		cancel()
	}()
	for {
		err := s.checkReady(ctx, c)
		if err == nil {
			s.running(c)
			return
		}
		s.mu.Lock()
		s.check = err
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(rd.Interval):
		}
	}
}

// running marks c ready, when it is still the program of the service.
func (s *service) running(c *child) {
	s.mu.Lock()
	if s.cur == c && s.status.State == "starting" {
		s.status.State = "running"
	}
	s.mu.Unlock()
	s.readyOnce.Do(func() { s.ready <- nil })
}

// checkReady checks once whether the service is ready.
func (s *service) checkReady(ctx context.Context, c *child) error {
	rd := s.spec.Ready
	ctx, cancel := context.WithTimeout(ctx, max(rd.Interval, time.Second))
	defer cancel()
	switch {
	case rd.HTTP != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rd.HTTP, nil)
		if err != nil {
			return err
		}
		// The check reaches the service itself, not a proxy, and a redirect
		// is an answer.
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("%s answered %s", rd.HTTP, resp.Status)
		}
		return nil
	case rd.TCP != "":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", rd.TCP)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	code, err := s.run(rd.Run, c, "ready", ctx.Done())
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("ready.run exited with %d", code)
	}
	return nil
}

// run runs prog, ready.run or stop.run, as a program of c: in a session of
// its own, with c's OPSTRUSS_SERVICE_ID and lock, its output in the
// service's log, and stopped when cancel is closed.
func (s *service) run(prog *plugin.Program, c *child, task string, cancel <-chan struct{}) (int, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	cmd := &exec.Cmd{Path: prog.Path, Args: prog.Args, Dir: prog.Dir, Env: append(slices.Clone(prog.Env), ServiceID+"="+c.p.ID),
		Stdout: outW, Stderr: outW, ExtraFiles: append([]*os.File{c.lock}, prog.LeaseFiles()...), SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	err = cmd.Start()
	_ = outW.Close()
	if err != nil {
		_ = outR.Close()
		return 0, err
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		s.copyLines(outR, task, "stdout")
	}()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	g := proc.Group(cmd.Process.Pid)
	select {
	case <-exited:
	case <-cancel:
		_ = g.Stop(syscall.SIGTERM, time.Second, nil)
		<-exited
	}
	_ = g.Stop(syscall.SIGTERM, time.Second, nil)
	select {
	case <-copied:
	case <-time.After(plugin.PipeWait):
	}
	_ = outR.Close()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ws.ExitStatus(), nil
}

// stop stops the service and waits until its loop ended.
func (s *service) stop() {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		close(s.stopped)
	}
	s.mu.Unlock()
	<-s.done
}

// halt stops c as Kubernetes stops a container with a preStop hook:
// stop.run first while it lives, then its signal to its process group when
// it still lives, all within its grace, then a kill.
func (s *service) halt(c *child) {
	deadline := time.Now().Add(s.spec.Stop.Grace)
	if s.spec.StopRun != nil && !closed(c.exited) {
		// stop.run ends with the grace, or at once on a kill.
		cancel := make(chan struct{})
		var once sync.Once
		end := func() { once.Do(func() { close(cancel) }) }
		t := time.AfterFunc(s.spec.Stop.Grace, end)
		ran := make(chan struct{})
		go func() {
			select {
			case <-s.sup.kill:
				end()
			case <-ran:
			}
		}()
		if code, err := s.run(s.spec.StopRun, c, "stop", cancel); err != nil {
			s.sup.event("warn", s.spec.Address, "stop.run: %v", err)
		} else if code != 0 {
			s.sup.event("warn", s.spec.Address, "stop.run exited with %d", code)
		}
		close(ran)
		t.Stop()
	}
	if err := proc.Group(c.p.PID).Stop(s.spec.Stop.Signal, max(time.Until(deadline), 0), s.sup.kill); err != nil {
		s.sup.event("warn", s.spec.Address, "%v", err)
	}
}

// cleanup stops what c left once its leader ended: the rest of its group,
// and the processes that carry its id in groups of their own; then its
// entry goes, once nothing holds its lock.
func (s *service) cleanup(c *child) {
	addr, stop := s.spec.Address, s.spec.Stop
	// A ready check of c stops with it, and holds its lock until then.
	c.helpers.Wait()
	if g := proc.Group(c.p.PID); !g.Empty() {
		s.sup.event("warn", addr, "stopping what pid %d left in its process group", c.p.PID)
		if err := g.Stop(stop.Signal, stop.Grace, s.sup.kill); err != nil {
			s.sup.event("warn", addr, "%v", err)
		}
	}
	if ps, err := proc.Find(ServiceID, c.p.ID); err == nil && len(ps) > 0 {
		s.sup.event("warn", addr, "stopping what pid %d left outside its process group: %v", c.p.PID, ps)
		var wg sync.WaitGroup
		for _, p := range ps {
			if g, err := proc.GroupOf(p.PID); err == nil {
				wg.Go(func() { _ = g.Stop(stop.Signal, stop.Grace, s.sup.kill) })
			}
		}
		wg.Wait()
	}
	ended := make(chan struct{})
	go func() {
		c.out.Wait()
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(plugin.PipeWait):
		s.sup.event("warn", addr, "a process outside the service's process group still holds its output; what it writes later is lost")
		for _, f := range c.outs {
			_ = f.Close()
		}
		<-ended
	}
	for _, f := range c.outs {
		_ = f.Close()
	}
	_ = c.lock.Close()
	s.mu.Lock()
	if s.cur == c {
		s.cur = nil
	}
	s.mu.Unlock()
	if l, err := survey(s.sup.root, c.p); err == nil {
		s.sup.settle(c.p, l)
	}
}

// removeFiles removes the files of the service's env: values, which live
// as long as it does.
func (s *service) removeFiles() {
	for path := range s.spec.Program.Files {
		_ = os.Remove(path)
	}
	_ = os.Remove(s.spec.Dir)
}

// newID is a new OPSTRUSS_SERVICE_ID.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func sortedValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
