package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/proc"
)

// Process is a program that the daemon guards, written in <stack>.json
// before it runs, so that what a daemon that ended left of it is found:
// its leader, which leads its session and process group, the id its
// processes carry, the lock they inherit, and how it stops.
type Process struct {
	Address string `json:"address"`
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Start   string `json:"start"` // when the leader started, from proc.StartTime
	Started string `json:"started"`
	Lock    string `json:"lock"` // relative to .opstruss
	Signal  string `json:"signal"`
	Grace   string `json:"grace"`
}

// registry is <stack>.json as the daemon keeps it: written whole each
// time a program starts or its entry goes.
type registry struct {
	path string
	mu   sync.Mutex
	f    File
}

func (r *registry) update(fn func(*File)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.f
	next.Processes, next.Schedules = slices.Clone(r.f.Processes), slices.Clone(r.f.Schedules)
	fn(&next)
	if err := writeFile(r.path, next); err != nil {
		return err
	}
	r.f = next
	return nil
}

func (r *registry) processes() []*Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.f.Processes)
}

// files is what the file holds now; its lists are copies.
func (r *registry) files() File {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.f
	f.Processes, f.Schedules = slices.Clone(f.Processes), slices.Clone(f.Schedules)
	return f
}

func without(ps []*Process, id string) []*Process {
	return slices.DeleteFunc(ps, func(p *Process) bool { return p.ID == id })
}

// removeLock removes the lock of an entry that went, and the directory of
// its service once nothing else is in it.
func removeLock(root string, p *Process) {
	path := filepath.Join(root, filepath.FromSlash(p.Lock))
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path))
}

// leftover is what is left of an entry: the process groups proven its
// own, which can be stopped, what is left that cannot be proven its own,
// and whether a process still holds its lock.
type leftover struct {
	stop    []proc.Group
	unclear string
	held    bool
}

// clear reports whether nothing of the entry is left.
func (l leftover) clear() bool { return len(l.stop) == 0 && l.unclear == "" && !l.held }

func (l leftover) String() string {
	var parts []string
	for _, g := range l.stop {
		parts = append(parts, fmt.Sprintf("process group %d", g))
	}
	return strings.Join(parts, ", ")
}

// survey finds what is left of p: the processes that carry its id with
// their groups, and its group while its leader is the one recorded, by
// pid and start time. A group whose leader is gone but which still has
// members in its session cannot be proven its own: nginx, for one, writes
// over its environment for the titles of its workers. The number of a
// group never moves to another session, so members in another one prove
// that the number was used again.
func survey(root string, p *Process) (leftover, error) {
	var l leftover
	carriers, held, err := proc.Marker{Name: ServiceID, Value: p.ID, Lock: filepath.Join(root, filepath.FromSlash(p.Lock))}.Alive()
	if err != nil {
		return l, err
	}
	groups := map[proc.Group]bool{}
	for _, c := range carriers {
		if g, err := proc.GroupOf(c.PID); err == nil {
			groups[g] = true
		}
	}
	leader := proc.Group(p.PID)
	switch start, err := proc.StartTime(p.PID); {
	case err == nil && start == p.Start:
		groups[leader] = true
	case err == nil:
		// The pid is another process's now, so the group it led is gone.
	case !groups[leader]:
		members, err := leader.Members()
		if err != nil {
			return l, err
		}
		for _, m := range members {
			if sid, err := proc.Session(m.PID); err == nil && sid == p.PID {
				l.unclear = fmt.Sprintf("%s is in the process group of pid %d, which is gone, and it does not show that it belongs to the service", m, p.PID)
				break
			}
		}
	}
	l.stop = slices.Sorted(maps.Keys(groups))
	l.held = held
	if len(l.stop) == 0 && l.unclear == "" && held {
		l.unclear = fmt.Sprintf("a process that the system does not show, such as a system program of macOS, holds the lock of pid %d", p.PID)
	}
	return l, nil
}

// halt stops the groups of what is left of p with its signal, and kills
// them after its grace. stop.run is not run: it needs the environment of
// the service, which only the daemon that ended had.
func (l leftover) halt(p *Process) {
	sig, err := proc.ParseSignal(p.Signal)
	if err != nil {
		sig = syscall.SIGTERM
	}
	grace, err := time.ParseDuration(p.Grace)
	if err != nil {
		grace = 10 * time.Second
	}
	var wg sync.WaitGroup
	for _, g := range l.stop {
		wg.Go(func() { _ = g.Stop(sig, grace) })
	}
	wg.Wait()
}

// Recover stops what a daemon of stack that ended left of the programs it
// guarded, those of the service at addr, or all when addr is empty, under
// the lock of the daemon, so none starts meanwhile, as opstruss stop does
// when no daemon runs. It returns what it stopped, and what is left that
// cannot be proven the programs', which stays recorded.
func Recover(root, stack, addr string) (stopped, unclear []string, err error) {
	err = held(root, stack, func(f *File) error {
		var keep []*Process
		for _, p := range f.Processes {
			if addr != "" && p.Address != addr {
				keep = append(keep, p)
				continue
			}
			l, err := survey(root, p)
			if err == nil && len(l.stop) > 0 {
				stopped = append(stopped, fmt.Sprintf("%s of %s", l, p.Address))
				l.halt(p)
				l, err = survey(root, p)
			}
			switch {
			case err != nil:
				unclear = append(unclear, fmt.Sprintf("%s: %v", p.Address, err))
			case l.unclear != "":
				unclear = append(unclear, fmt.Sprintf("%s: %s", p.Address, l.unclear))
			case len(l.stop) > 0:
				unclear = append(unclear, fmt.Sprintf("%s: %s still has processes after a kill", p.Address, l))
			default:
				removeLock(root, p)
				continue
			}
			keep = append(keep, p)
		}
		f.Processes = keep
		return nil
	})
	return stopped, unclear, err
}

// Forget forgets the entries of the service at addr that cannot be proven
// its own, under the lock of the daemon of stack, and returns them.
func Forget(root, stack, addr string) (forgot []string, err error) {
	err = held(root, stack, func(f *File) error {
		var keep []*Process
		for _, p := range f.Processes {
			if p.Address == addr {
				l, err := survey(root, p)
				if err == nil && l.unclear != "" && len(l.stop) == 0 {
					forgot = append(forgot, l.unclear)
					removeLock(root, p)
					continue
				}
			}
			keep = append(keep, p)
		}
		f.Processes = keep
		return nil
	})
	return forgot, err
}

// held runs fn on <stack>.json under the lock of the daemon of stack,
// which no daemon then holds, and writes what fn leaves.
func held(root, stack string, fn func(*File) error) error {
	lock, err := os.OpenFile(lockPath(root, stack), os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			running := &RunningError{Stack: stack}
			if f, err := Read(root, stack); err == nil {
				running.PID = f.PID
			}
			return running
		}
		return err
	}
	f, err := Read(root, stack)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	before := len(f.Processes)
	if err := fn(f); err != nil {
		return err
	}
	if len(f.Processes) == before {
		return nil
	}
	return writeFile(filePath(root, stack), f)
}

// status says how the service at addr runs in a daemon of stack in
// another process, from what <stack>.json records of it.
func status(root, stack, addr string) (engine.ServiceStatus, bool) {
	if !Alive(root, stack) {
		return engine.ServiceStatus{}, false
	}
	f, err := Read(root, stack)
	if err != nil {
		return engine.ServiceStatus{}, false
	}
	for _, p := range slices.Backward(f.Processes) {
		if p.Address != addr {
			continue
		}
		if start, err := proc.StartTime(p.PID); err == nil && start == p.Start {
			since, _ := time.Parse(time.RFC3339, p.Started)
			return engine.ServiceStatus{State: "running", PID: p.PID, Since: since}, true
		}
	}
	return engine.ServiceStatus{State: "stopped"}, true
}
