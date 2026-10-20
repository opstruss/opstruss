package state

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// started is when this process started, kept with every lock it takes.
var started = time.Now().UTC()

// HostID returns the random id of this machine for this user, kept in
// ~/.opstruss/host-id and made the first time; hostnames can repeat across
// machines, so locks name their holder by this id.
func HostID() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".opstruss", "host-id")
	data, err := os.ReadFile(p)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	id := newToken()
	tmp, err := writeTemp(filepath.Dir(p), []byte(id+"\n"))
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	// Two processes that start together agree on the id that won the link.
	if err := os.Link(tmp, p); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	data, err = os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// NewHolder describes this process as the holder of a lock for run.
func NewHolder(run, hostID string) Holder {
	h := Holder{Version: HolderVersion, Run: run, Host: hostID, PID: os.Getpid(),
		Started: started.Format(time.RFC3339Nano), Since: time.Now().UTC().Format(time.RFC3339)}
	h.Hostname, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		h.User = u.Username
	}
	return h
}

// NewRunID names a run by its UTC start time and 8 random hex digits, so
// runs list in order and never share a name.
func NewRunID() string {
	return time.Now().UTC().Format("20060102T150405Z") + "-" + newToken()[:8]
}
