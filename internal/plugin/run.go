// Package plugin runs programs under the program contract: the clean
// environment, the facts and contract files that each kind of program
// receives, the local executor, and the parsing of what a program writes
// back.
package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
)

// Contract is the version of the program contract and of the backend
// protocol, in OPSTRUSS_CONTRACT; it rises only for a change that is not
// compatible.
const Contract = "1"

const (
	// MaxOutputs is the most that the outputs of one unit or task take as
	// JSON, since state keeps them; it also bounds OPSTRUSS_OUTPUT,
	// OPSTRUSS_PLAN, and each stream or file that an output reads.
	MaxOutputs = 1 << 20
	// MaxInputEnv is the largest input that INPUT_<NAME> holds; a larger
	// one is only in OPSTRUSS_INPUTS, since Linux limits a variable to
	// 128 KiB.
	MaxInputEnv = 64 << 10
	// MaxResult bounds OPSTRUSS_RESULT.
	MaxResult = 64 << 10
	// Tail is how much of a stream that no output reads a call keeps, for
	// the stdout and stderr of a step.
	Tail = 64 << 10
	// PipeWait is how long a call waits for the end of its output once its
	// process group is empty, since a process that left the group can
	// still hold it.
	PipeWait = 2 * time.Second
)

// lockName is the lock in a run's temporary directory that every program
// of the run inherits.
const lockName = ".run.lock"

// Stop is how a program stops when its run is cancelled or times out.
type Stop struct {
	Signal syscall.Signal
	Grace  time.Duration
}

// DefaultStop is how the program of a run unit or task stops by default,
// and ServiceStop how a service does, as docker stop does.
var (
	DefaultStop = Stop{Signal: syscall.SIGINT, Grace: time.Minute}
	ServiceStop = Stop{Signal: syscall.SIGTERM, Grace: 10 * time.Second}
)

// StopOf reads stop:, with the defaults for what it leaves out.
func StopOf(s *model.Stop) (Stop, error) { return StopWith(s, DefaultStop) }

// StopWith reads stop:, with def for what it leaves out.
func StopWith(s *model.Stop, def Stop) (Stop, error) {
	stop := def
	if s == nil {
		return stop, nil
	}
	if s.Signal != "" {
		sig, err := proc.ParseSignal(s.Signal)
		if err != nil {
			return Stop{}, err
		}
		stop.Signal = sig
	}
	if s.Grace != "" {
		d, err := time.ParseDuration(s.Grace)
		if err != nil {
			return Stop{}, fmt.Errorf("stop.grace: %w", err)
		}
		stop.Grace = d
	}
	return stop, nil
}

// Run is what the programs of one run share.
type Run struct {
	Project   string
	Stack     string
	ID        string
	Parents   []string // ids of the runs that started this one, nearest last
	Temp      string   // .opstruss/tmp/<run>/: OPSTRUSS_TEMP, also the contract files of each call
	Artifacts string   // OPSTRUSS_ARTIFACTS
	Environ   []string // the operator's environment, read once when the run starts

	operator map[string]string
	base     map[string]string
	tracker  *proc.Tracker
	calls    atomic.Int64
}

// Open makes the run's directories and takes the lock that every program
// of the run inherits, which shows a later run that programs of this one
// still live after its OpsTruss was killed.
func (r *Run) Open() error {
	for _, p := range []*string{&r.Temp, &r.Artifacts} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return err
		}
		*p = abs
	}
	t, err := proc.Track(filepath.Join(r.Temp, lockName))
	if err != nil {
		return err
	}
	r.tracker = t
	r.operator = map[string]string{}
	for _, kv := range r.Environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			r.operator[k] = v
		}
	}
	r.base = clean(r.operator)
	return nil
}

// Close removes the run's temporary directory, which its programs shared,
// and lets go of the run's lock.
func (r *Run) Close() error {
	err := os.RemoveAll(r.Temp)
	if cerr := r.tracker.Close(); err == nil {
		err = cerr
	}
	return err
}

// Marker finds the programs of the run id, whose temporary directory is
// temp, after its OpsTruss is gone.
func Marker(id, temp string) proc.Marker {
	return proc.Marker{Name: "OPSTRUSS_RUN", Value: id, Lock: filepath.Join(temp, lockName)}
}
