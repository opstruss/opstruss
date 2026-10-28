package record

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/state"
)

// MaxLog is the size at which a log turns over, and LogFiles how many files
// of it are kept, as the json-file driver of Docker keeps them.
const (
	MaxLog   = 10 << 20
	LogFiles = 3
)

// logMax is MaxLog, smaller in tests.
var logMax int64 = MaxLog

// Log is a file of events outside runs, such as those of a daemon: one
// event per line, numbered by seq within the file. A full file moves to
// <path>.1, and that one to <path>.2, as logrotate names them, so a reader
// that follows <name>.jsonl does not take the old files for new ones.
type Log struct {
	path   string
	mu     sync.Mutex
	f      *os.File
	size   int64
	seq    int64
	closed bool
	lost   int64 // events that could not be written since err
	err    error
}

// OpenLog opens the log at path to append to it after the events already
// there; a line that a crash cut short is dropped.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the file at the path of the log to append to it.
func (l *Log) open() error {
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if l.size, l.seq, err = lastLine(f); err != nil {
		_ = f.Close()
		return err
	}
	l.f = f
	return nil
}

// lastLine reads the seq of the last whole line of f, and cuts what
// follows it.
func lastLine(f *os.File) (size, seq int64, err error) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	size = info.Size()
	buf := make([]byte, min(size, 64<<10))
	if _, err := f.ReadAt(buf, size-int64(len(buf))); err != nil && err != io.EOF {
		return 0, 0, err
	}
	end := bytes.LastIndexByte(buf, '\n')
	if keep := size - int64(len(buf)) + int64(end) + 1; keep != size {
		if err := f.Truncate(keep); err != nil {
			return 0, 0, err
		}
		size = keep
	}
	if end < 0 {
		return size, 0, nil
	}
	var ev struct {
		Seq int64 `json:"seq"`
	}
	_ = json.Unmarshal(buf[bytes.LastIndexByte(buf[:end], '\n')+1:end], &ev)
	return size, ev.Seq, nil
}

// Emit gives ev its envelope and appends it as one write, so a reader
// never sees half of it, and returns it with that envelope. An event that
// cannot be written is lost, as the output of a program that fills the
// disk is; the first one written after says how many were lost, and why.
func (l *Log) Emit(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	lost := func(err error) Event {
		if l.lost == 0 {
			l.err = err
		}
		l.lost++
		ev.V, ev.Seq, ev.Time = EventVersion, l.seq+1, now()
		return ev
	}
	if l.closed {
		return lost(os.ErrClosed)
	}
	// A file that a turn could not open is opened again.
	if l.f == nil {
		if err := l.open(); err != nil {
			return lost(err)
		}
	}
	if l.lost > 0 {
		msg := "an event of this log was not stored"
		if l.lost > 1 {
			msg = fmt.Sprintf("%d events of this log were not stored", l.lost)
		}
		note := Event{Type: "diagnostic", Level: "warn", Message: fmt.Sprintf("%s: %v", msg, l.err)}
		if _, err := l.write(note); err != nil {
			return lost(err)
		}
		l.lost, l.err = 0, nil
	}
	ev, err := l.write(ev)
	if err != nil {
		return lost(err)
	}
	return ev
}

// write gives ev its envelope and appends it, after the full file turned
// over. A write that fails leaves no part of its line.
func (l *Log) write(ev Event) (Event, error) {
	ev.V, ev.Seq, ev.Time = EventVersion, l.seq+1, now()
	data, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	if l.size > 0 && l.size+int64(len(data))+1 > logMax {
		if err := l.turn(); err != nil {
			return ev, err
		}
		ev.Seq = l.seq + 1
		if data, err = json.Marshal(ev); err != nil {
			return ev, err
		}
	}
	if _, err := l.f.Write(append(data, '\n')); err != nil {
		_ = l.f.Truncate(l.size)
		return ev, err
	}
	l.size += int64(len(data)) + 1
	l.seq = ev.Seq
	return ev, nil
}

// turn moves the full file aside, over the oldest one, and starts a new
// one.
func (l *Log) turn() error {
	_ = l.f.Close()
	l.f = nil
	for i := LogFiles - 1; i > 0; i-- {
		from := l.path
		if i > 1 {
			from = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		_ = os.Rename(from, fmt.Sprintf("%s.%d", l.path, i))
	}
	return l.open()
}

// Close closes the file of the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// FollowEvery is how often a reader that follows a log or a run looks for
// what was appended.
var FollowEvery = 200 * time.Millisecond

// ReadLog hands the events of the log at path to fn, oldest first,
// starting with the files it turned over into. With follow, it goes on
// with what is appended, also after the log turns over, until ctx ends, as
// tail -F does; a log that does not exist yet is waited for.
func ReadLog(ctx context.Context, path string, follow bool, fn func(Event)) error {
	// The file at path is opened first: one that turns over while the older
	// files are read is among them then, and is read once, after them.
	f, err := os.Open(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	held, err := infoOf(f)
	if err != nil {
		return err
	}
	if err := readTurned(path, nil, held, fn); err != nil {
		return err
	}
	var rest []byte
	for {
		if f == nil {
			if f, err = os.Open(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			rest = nil
		}
		if f != nil {
			if err := readEvents(f, &rest, fn); err != nil {
				return err
			}
			// A log that turned over is another file at its path now; what
			// the old one got last is read, then the files it turned over
			// into since, then the new one.
			if now, err := os.Stat(path); err == nil {
				if info, err := f.Stat(); err == nil && !os.SameFile(info, now) {
					if err := readEvents(f, &rest, fn); err != nil {
						return err
					}
					_ = f.Close()
					if f, err = os.Open(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
					rest = nil
					if held, err = infoOf(f); err != nil {
						return err
					}
					if err := readTurned(path, info, held, fn); err != nil {
						return err
					}
					continue
				}
			}
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(FollowEvery):
		}
	}
}

// infoOf is the FileInfo of f, or nil for no file.
func infoOf(f *os.File) (os.FileInfo, error) {
	if f == nil {
		return nil, nil
	}
	return f.Stat()
}

// readTurned hands fn the events of the files that the log at path turned
// over into, oldest first: those newer than after, or all when after is
// nil, except held, the file at path, which the caller reads next. The
// files are opened newest first, so one that turns over meanwhile is seen
// twice rather than missed, and read once.
func readTurned(path string, after, held os.FileInfo, fn func(Event)) error {
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	seen := func(info os.FileInfo) bool {
		if held != nil && os.SameFile(info, held) {
			return true
		}
		return slices.ContainsFunc(files, func(f *os.File) bool {
			other, err := f.Stat()
			return err == nil && os.SameFile(info, other)
		})
	}
scan:
	for i := 1; i < LogFiles; i++ {
		f, err := os.Open(fmt.Sprintf("%s.%d", path, i))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		switch {
		case err != nil:
			_ = f.Close()
			return err
		case after != nil && os.SameFile(info, after):
			_ = f.Close()
			break scan
		case seen(info):
			_ = f.Close()
		default:
			files = append(files, f)
		}
	}
	for i := len(files) - 1; i >= 0; i-- {
		var rest []byte
		if err := readEvents(files[i], &rest, fn); err != nil {
			return err
		}
	}
	return nil
}

// readEvents hands each whole line that f has from where it is to fn as an
// event; a line that is not one, as one that a full disk cut, is skipped,
// and an unfinished one waits in rest for the rest of it. An event of a
// newer version stops it.
func readEvents(f *os.File, rest *[]byte, fn func(Event)) error {
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		chunk, err := r.ReadSlice('\n')
		*rest = append(*rest, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == nil {
			line := bytes.TrimSpace(*rest)
			var ev Event
			var newer *state.NewerError
			if err := state.CheckVersion(line, "v", "an event of "+f.Name(), EventVersion); errors.As(err, &newer) {
				return err
			} else if err == nil && json.Unmarshal(line, &ev) == nil {
				fn(ev)
			}
			*rest = (*rest)[:0]
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// Stamp writes t as the times of runs and their events are written: UTC,
// to the millisecond, so that one run reads its start before its end.
func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// now is the time of an event.
func now() string { return Stamp(time.Now()) }
