// Package state keeps the records of units: the backend contract, the
// local backend, versioned records, and the _meta object of each stack.
package state

import (
	"errors"
	"fmt"
	"time"
)

// Backend is the contract of every backend: six operations on keys that
// follow the layout of the state, such as prod/_meta, prod/web/state.json,
// and prod/web/state.lock.
//
// Put and Delete are conditional: ifMatch is the etag that Get returned
// after the lock was taken, and an empty ifMatch means the key must not
// exist yet. Lock does not wait: a held key returns *LockedError, and the
// engine waits with backoff. Unlock takes the token that Lock returned,
// so a holder whose lock was taken over cannot release the new one.
type Backend interface {
	Get(key string) (data []byte, etag string, err error)
	Put(key string, data []byte, ifMatch string) (etag string, err error)
	Delete(key string, ifMatch string) error
	List(prefix string) ([]string, error)
	Lock(key string, h Holder) (token string, err error)
	Unlock(key, token string) error
}

// Scanner is a backend that gives every key under a prefix with its content
// in one operation, as list gives the keys: the capability scan of a plugin
// backend. One without it returns errors.ErrUnsupported, and the store
// lists and gets instead.
type Scanner interface {
	Scan(prefix string) (map[string][]byte, error)
}

var (
	// ErrNotExist is the error of Get and Delete on a key that is absent.
	ErrNotExist = errors.New("no such key")
	// ErrConflict means the key changed since its etag was read, or exists
	// when it must not.
	ErrConflict = errors.New("the key changed since it was read")
	// ErrNotHolder means Unlock was given a token the lock does not have.
	ErrNotHolder = errors.New("the lock belongs to another holder")
)

// HolderVersion is the version of a holder, which a local state.lock holds.
const HolderVersion = 1

// DaemonRun is the run of the holder of the lease of a daemon, which is no
// run of its own.
const DaemonRun = "daemon"

// Holder says who holds a lock.
type Holder struct {
	Version  int    `json:"version"`
	Run      string `json:"run"`
	Host     string `json:"host"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	PID      int    `json:"pid"`
	// Start is when the process started, as the system keeps it: it only
	// compares with the start of the same pid on the same host, which a pid
	// that another process took has another of.
	Start   string `json:"start,omitempty"`
	Started string `json:"started"`
	Since   string `json:"since"`
}

func (h Holder) String() string {
	if h.Run == "" && h.PID == 0 {
		return "another run"
	}
	who := "run " + h.Run
	if h.Run == DaemonRun {
		who = "the daemon"
	}
	s := fmt.Sprintf("%s (pid %d on %s", who, h.PID, h.Hostname)
	if since, err := time.Parse(time.RFC3339, h.Since); err == nil {
		s += fmt.Sprintf(", for %s", time.Since(since).Round(time.Second))
	}
	return s + ")"
}

// LockedError is the error of Lock on a key that another holder has.
// Token is that holder's, with which a lock proven stale is released; a
// local lock, which is never stale, has none.
type LockedError struct {
	Key    string
	Holder Holder
	Token  string
}

func (e *LockedError) Error() string { return fmt.Sprintf("%s is locked by %s", e.Key, e.Holder) }
