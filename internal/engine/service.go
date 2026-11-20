package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/plugin"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/opstruss/opstruss/internal/record"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/state"
)

// Supervisor keeps the services of a stack. In the daemon of the stack it
// is the daemon's own; in the process of the command line it makes that
// process the daemon of the stack once a service starts, as up stays in
// the foreground while its services run.
type Supervisor interface {
	// Start stops the service at s.Address when it runs, starts s, and
	// returns once s is ready.
	Start(ctx context.Context, s *Service) error
	// Stop stops the service at addr, or what a daemon that ended left of
	// it, and forgets it.
	Stop(ctx context.Context, addr string) error
	// Status says how the service at addr runs, or false when the
	// supervisor does not know it.
	Status(addr string) (ServiceStatus, bool)
	// Forget forgets what is left of the service at addr that cannot be
	// proven its own, as opstruss unlock does, and says what that was.
	Forget(addr string) ([]string, error)
	// CheckBackend says why a run that keeps the state of the stack in
	// backend, as the files name it now, cannot run here, or nil: a daemon
	// keeps the triggers and services of its stack with the lease that it
	// took from one backend, until it starts again.
	CheckBackend(backend string) error
}

// Service is a service as its supervisor runs it: the program, and those
// of ready.run and stop.run, prepared with the values of the run that
// starts it, which the supervisor keeps for restarts.
type Service struct {
	RetainSources func() func()

	Address string
	Dir     string // daemon/<stack>/<unit>/: the files of its env: values and the locks its programs inherit
	Program *plugin.Program
	Restart string // no, on-failure, or always
	Stop    plugin.Stop
	StopRun *plugin.Program
	Ready   Ready
	Redact  *secret.Redactor // hides the sensitive values of the run in its log
	// Rebuild prepares the program again before each start after the
	// first, for a watch source whose inputs are read each time.
	Rebuild func() (*plugin.Program, error)
	// Lines gets each line of output in place of the log, for a watch
	// source, whose lines are its events.
	Lines func(stream, line string)
}

// Ready says when a service is ready: when one of HTTP, TCP, and Run
// passes, or as soon as it started when none is given.
type Ready struct {
	HTTP     string
	TCP      string
	Run      *plugin.Program
	Interval time.Duration
	Timeout  time.Duration
}

// ServiceStatus is how a service runs: its state, running, starting (not
// ready yet), restarting, or stopped, its process, since when, and how
// often it restarted.
type ServiceStatus struct {
	State    string    `json:"state"`
	PID      int       `json:"pid,omitzero"`
	Since    time.Time `json:"since,omitzero"`
	Restarts int       `json:"restarts,omitzero"`
	ExitCode int       `json:"exit_code,omitzero"` // of the last program that exited, once one did
	Exited   time.Time `json:"exited,omitzero"`    // when it exited
	Next     time.Time `json:"next,omitzero"`      // when it starts again, while it waits to
}

// Defaults of ready: and of the restart policy.
const (
	DefaultReadyInterval = time.Second
	DefaultReadyTimeout  = time.Minute
	DefaultRestart       = "on-failure"
)

// serviceDef is what a service unit adds to a run unit, evaluated.
type serviceDef struct {
	restart  string
	ready    model.Ready // http and tcp evaluated
	readyRun *model.Command
	stopRun  *model.Command
	interval time.Duration
	timeout  time.Duration
}

// resolveService evaluates the program of a service unit and what it
// adds: ready: and stop.run.
func (e *engine) resolveService(u *model.Unit, r *resolved, s *expr.Scope) error {
	var err error
	if r.cmd, err = e.command(u.Service, s); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	d := &serviceDef{restart: cmp.Or(u.Restart, DefaultRestart), interval: DefaultReadyInterval, timeout: DefaultReadyTimeout}
	if rd := u.Ready; rd != nil {
		if d.ready.HTTP, err = expr.EvalText(rd.HTTP, s); err != nil {
			return fmt.Errorf("ready.http: %w", err)
		}
		if d.ready.TCP, err = expr.EvalText(rd.TCP, s); err != nil {
			return fmt.Errorf("ready.tcp: %w", err)
		}
		if rd.Run != nil {
			cmd, err := e.command(rd.Run, s)
			if err != nil {
				return fmt.Errorf("ready.run: %w", err)
			}
			d.readyRun = &cmd
		}
		if rd.Interval != "" {
			if d.interval, err = time.ParseDuration(rd.Interval); err != nil {
				return fmt.Errorf("ready.interval: %w", err)
			}
		}
		if rd.Timeout != "" {
			if d.timeout, err = time.ParseDuration(rd.Timeout); err != nil {
				return fmt.Errorf("ready.timeout: %w", err)
			}
		}
	}
	if st := u.Stop; st != nil && st.Run != nil {
		cmd, err := e.command(st.Run, s)
		if err != nil {
			return fmt.Errorf("stop.run: %w", err)
		}
		d.stopRun = &cmd
	}
	if r.stop, err = plugin.StopWith(u.Stop, plugin.ServiceStop); err != nil {
		return err
	}
	r.service = d
	return nil
}

// digest adds to the digest trigger of a service what decides how its
// supervisor runs it, so a change restarts it.
func (d *serviceDef) digest(r *resolved, def map[string]any) {
	ready := map[string]any{"interval": d.interval.String(), "timeout": d.timeout.String()}
	switch {
	case d.ready.HTTP != "":
		ready["http"] = d.ready.HTTP
	case d.ready.TCP != "":
		ready["tcp"] = d.ready.TCP
	case d.readyRun != nil:
		ready["run"] = cmdValue(*d.readyRun)
	default:
		ready = nil
	}
	stop := map[string]any{"signal": proc.SignalName(r.stop.Signal), "grace": r.stop.Grace.String()}
	if d.stopRun != nil {
		stop["run"] = cmdValue(*d.stopRun)
	}
	def["kind"], def["restart"], def["stop"] = "service", d.restart, stop
	if ready != nil {
		def["ready"] = ready
	}
}

// destroy is the destroy contract of a service: how it stops.
func (d *serviceDef) destroy(r *resolved) *state.Destroy {
	stop := &state.DestroyStop{Signal: proc.SignalName(r.stop.Signal), Grace: r.stop.Grace.String()}
	destroy := &state.Destroy{Stop: stop}
	if d.stopRun != nil {
		stop.Run = cmdValue(*d.stopRun)
		if d.stopRun.Script != "" {
			destroy.Shell = r.shell.Value()
		}
	}
	return destroy
}

// services is the supervisor of the run's services, or one that knows of
// none.
func (e *engine) services() Supervisor {
	if e.opts.Supervisor != nil {
		return e.opts.Supervisor
	}
	return noSupervisor{}
}

// running reports whether the service at addr runs, or starts or
// restarts.
func (e *engine) running(addr string) bool {
	st, ok := e.services().Status(addr)
	return ok && st.State != "stopped"
}

// serviceOf prepares the programs of a service unit with the values of
// this run.
func (e *engine) serviceOf(inst *instance, r *resolved, rec *state.Record) (*Service, error) {
	d := r.service
	s := &Service{RetainSources: e.p.Sources.Retain, Address: inst.address, Restart: d.restart, Stop: r.stop, Redact: e.redact,
		Dir:   filepath.Join(e.root, "daemon", e.p.StackName, filepath.FromSlash(e.short(inst.address))),
		Ready: Ready{HTTP: d.ready.HTTP, TCP: d.ready.TCP, Interval: d.interval, Timeout: d.timeout}}
	prepare := func(cmd model.Command, stdin []byte) (*plugin.Program, error) {
		return e.run.Prepare(&plugin.Call{Command: cmd, Shell: r.shell, Base: inst.dir, Dir: r.dir, Env: r.env,
			Pass: slices.Sorted(maps.Keys(r.pass)), Stdin: stdin, Unit: &plugin.Unit{Address: inst.address, StateKey: rec.StateKey}}, s.Dir)
	}
	var err error
	if s.Program, err = prepare(r.cmd, r.stdin); err != nil {
		return nil, err
	}
	if d.readyRun != nil {
		if s.Ready.Run, err = prepare(*d.readyRun, nil); err != nil {
			return nil, fmt.Errorf("ready.run: %w", err)
		}
	}
	if d.stopRun != nil {
		if s.StopRun, err = prepare(*d.stopRun, nil); err != nil {
			return nil, fmt.Errorf("stop.run: %w", err)
		}
	}
	return s, nil
}

// startService runs the turn of a service unit: its supervisor stops the
// one that runs, starts this one, and waits until it is ready.
func (e *engine) startService(ctx context.Context, inst *instance, r *resolved, rec *state.Record) (*plugin.Result, error) {
	s, err := e.serviceOf(inst, r, rec)
	if err != nil {
		return nil, err
	}
	sup := e.services()
	if err := sup.Start(ctx, s); err != nil {
		return nil, err
	}
	if st, ok := sup.Status(inst.address); ok {
		e.say("unit", inst.address, "%s, pid %d", st.State, st.PID)
	}
	return &plugin.Result{Changed: true}, nil
}

// serviceElsewhere refuses a service that the daemon of another checkout
// keeps, on this host or another, as its record says in a stack that
// shares its state through a plugin backend: only that daemon has the
// service and the notes of what is left of it.
func (e *engine) serviceElsewhere(addr string, rec *state.Record) error {
	if _, shared := e.store.Backend.(*pluginBackend); !shared || rec == nil || rec.Host == "" || rec.Host == e.host && rec.Checkout == e.p.Dir {
		return nil
	}
	short := e.short(addr)
	return fmt.Errorf("the service %s runs from %s on %s, whose daemon keeps it; up and destroy it there, or, once that checkout is gone for good, forget its record with opstruss state rm %s",
		short, rec.Checkout, cmp.Or(rec.Hostname, "another host"), short)
}

// serviceStatus is the status of a service unit: its state, pid, since
// when, how often it restarted, how its last program exited and when, and
// when it starts again.
func (e *engine) serviceStatus(inst *instance) map[string]any {
	out := map[string]any{"state": "absent", "pid": int64(0), "since": "", "restarts": int64(0), "exit_code": int64(0), "exited": "", "next": ""}
	if rec := inst.record; rec != nil && rec.Tombstone == nil {
		out["state"] = "stopped"
	}
	if st, ok := e.services().Status(inst.address); ok {
		out["state"], out["pid"], out["restarts"], out["exit_code"] = st.State, int64(st.PID), int64(st.Restarts), int64(st.ExitCode)
		if !st.Since.IsZero() {
			out["since"] = st.Since.UTC().Format(time.RFC3339)
		}
		// To the millisecond, as the time of an event: the first restarts
		// come within a second.
		for k, t := range map[string]time.Time{"exited": st.Exited, "next": st.Next} {
			if !t.IsZero() {
				out[k] = record.Stamp(t)
			}
		}
	}
	return out
}

// noSupervisor is the supervisor of a run that cannot start services.
type noSupervisor struct{}

var errNoSupervisor = errors.New("this run has no supervisor for services")

func (noSupervisor) Start(context.Context, *Service) error { return errNoSupervisor }
func (noSupervisor) Stop(context.Context, string) error    { return nil }
func (noSupervisor) Status(string) (ServiceStatus, bool)   { return ServiceStatus{}, false }
func (noSupervisor) Forget(string) ([]string, error)       { return nil, nil }
func (noSupervisor) CheckBackend(string) error             { return nil }

// Unlock forgets what is left of the service of the one unit named that
// cannot be proven its own, such as a process group whose leader is gone,
// which keeps up from starting it again, and says what it forgot; the
// record of the run keeps that.
func Unlock(ctx context.Context, opts Options) (*Result, error) {
	e, err := open(ctx, opts, "unlock", "")
	if err != nil {
		return nil, err
	}
	defer e.releaseSources()
	if err := e.unlock(); err != nil {
		e.fail("", err)
	}
	e.close(map[int]string{0: success, 1: failure}[e.exit], nil, nil)
	return e.result(), nil
}

func (e *engine) unlock() error {
	if len(e.opts.Units) != 1 {
		return errors.New("unlock takes the name of one unit, or _daemon, _kv, or _meta")
	}
	name, scope := e.opts.Units[0], e.p.StackName
	addr := scope + "/" + name
	var key string
	switch name {
	case "_daemon":
		if e.p.Global {
			return errors.New("the global scope has no daemon, so no lease of one")
		}
		key = state.DaemonLockKey(scope)
	case "_kv":
		key = state.KVLockKey(scope)
	case "_meta":
		key = state.MetaLockKey(scope)
	default:
		key = state.LockKey(addr)
	}
	var did []string
	released, err := e.releaseStale(addr, key)
	if err != nil {
		return err
	}
	if released != "" {
		did = append(did, released)
	}
	if !strings.HasPrefix(name, "_") {
		forgot, err := e.services().Forget(addr)
		if err != nil {
			return err
		}
		for _, f := range forgot {
			e.say("unit", addr, "forgot %s", f)
			did = append(did, "forgot "+f)
		}
	}
	if len(did) == 0 {
		if strings.HasPrefix(name, "_") {
			return fmt.Errorf("%s is not locked, so there is nothing to release", name)
		}
		return fmt.Errorf("%s is not locked, and nothing is left of its service that is not proven its own, so there is nothing to forget", name)
	}
	e.touch(name, &record.Unit{Outcome: success, Changed: true, Plan: "unlock: " + strings.Join(did, "; ")})
	return nil
}

// releaseStale releases the lock key of a plugin backend whose holder is
// gone, as the holder's token: one that died on this host at once, one of
// another host once the operator agrees. It says what it did, or "" for a
// lock that nobody holds; a local lock is never stale.
func (e *engine) releaseStale(addr, key string) (string, error) {
	pb, ok := e.store.Backend.(*pluginBackend)
	if !ok {
		return "", nil
	}
	token, err := pb.tryLock(key, e.holder)
	var le *state.LockedError
	switch {
	case err == nil:
		return "", pb.Unlock(key, token)
	case !errors.As(err, &le):
		return "", err
	}
	h := le.Holder
	switch {
	case h.Host == e.host && !e.holderDead(h):
		return "", fmt.Errorf("the lock of %s is held by %s, which still runs on this host; wait for it, or stop it", addr, h)
	case h.Host != e.host && !e.opts.Yes:
		q := Question{Op: "unlock", Address: addr, Summary: "locked by " + h.String()}
		ok, err := false, error(nil)
		if e.opts.Approve != nil {
			ok, err = e.opts.Approve(e.ctx, q)
		}
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("the lock of %s is held by %s on another host, whose programs may still run; once you are sure they stopped, release it with opstruss unlock --yes", addr, h)
		}
	}
	if err := pb.Unlock(key, le.Token); err != nil {
		if errors.Is(err, state.ErrNotHolder) {
			return "", fmt.Errorf("the lock of %s changed hands meanwhile; run unlock again to see who holds it now", addr)
		}
		return "", err
	}
	e.say("unit", addr, "released the lock that %s held", h)
	return "released the lock of " + h.String(), nil
}
