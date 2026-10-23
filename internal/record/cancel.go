package record

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// cancelFile holds the requests to cancel a run, one JSON line each, in
// the run's directory: whoever asks appends one, and the run reads them
// while it lives, so a request reaches that run alone, in whatever process
// runs it, and never a process that took its pid.
const cancelFile = "cancel.jsonl"

// maxCancelRead bounds what a run reads of cancel.jsonl at a time.
const maxCancelRead = 64 << 10

// Cancel is a request to cancel a run: who asks, why, and whether its
// programs are killed at once instead of stopped within their stop.grace.
type Cancel struct {
	Time     string `json:"time"`
	Reason   string `json:"reason"` // requested: opstruss runs cancel; concurrency: a call with concurrency: cancel
	Force    bool   `json:"force,omitempty"`
	User     string `json:"user,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Run      string `json:"run,omitempty"` // the run that asks, when a run does
}

// ErrEnded is a request for a run that no longer runs.
var ErrEnded = errors.New("the run ended")

// RequestCancel asks the run in dir to cancel, while it still runs.
func RequestCancel(dir string, c Cancel) error {
	if runEnded(dir) {
		return ErrEnded
	}
	c.Time = now()
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, cancelFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	// One short write in append mode, so lines of two callers do not mix.
	_, err = f.Write(append(data, '\n'))
	return errors.Join(err, f.Close())
}

// WatchCancel hands fn each request to cancel the run, as cancel.jsonl
// gains them, within flushEvery, until the run closes.
func (r *Run) WatchCancel(fn func(Cancel)) {
	path := filepath.Join(r.Dir, cancelFile)
	go func() {
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		var off int64
		for {
			select {
			case <-r.done:
				return
			case <-t.C:
				off = readCancels(path, off, fn)
			}
		}
	}()
}

// readCancels hands fn the whole lines of path from off, and returns where
// the next read starts.
func readCancels(path string, off int64, fn func(Cancel)) int64 {
	f, err := os.Open(path)
	if err != nil {
		return off
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.NewSectionReader(f, off, maxCancelRead))
	if err != nil {
		return off
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		if len(data) == maxCancelRead {
			// A line longer than any request: skip it.
			return off + int64(len(data))
		}
		return off
	}
	sc := bufio.NewScanner(bytes.NewReader(data[:end+1]))
	sc.Buffer(nil, maxCancelRead)
	for sc.Scan() {
		var c Cancel
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			fn(c)
		}
	}
	return off + int64(end) + 1
}

// WaitEnded waits until the run in dir ends, or ctx does.
func WaitEnded(ctx context.Context, dir string) error {
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for !runEnded(dir) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}
