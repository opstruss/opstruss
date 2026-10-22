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
	"sync"
	"time"
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
	path string
	mu   sync.Mutex
	f    *os.File
	size int64
	seq  int64
}

// OpenLog opens the log at path to append to it after the events already
// there; a line that a crash cut short is dropped.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Log{path: path, f: f}
	if l.size, l.seq, err = lastLine(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
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
// never sees half of it, and returns it with that envelope. A log that
// cannot be written loses the event, as the output of a program that fills
// the disk does.
func (l *Log) Emit(ev Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev.V, ev.Seq, ev.Time = EventVersion, l.seq+1, now()
	if l.f == nil {
		return ev
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return ev
	}
	if l.size > 0 && l.size+int64(len(data))+1 > logMax {
		l.turn()
		if l.f == nil {
			return ev
		}
		ev.Seq = 1
		data, _ = json.Marshal(ev)
	}
	l.seq = ev.Seq
	n, _ := l.f.Write(append(data, '\n'))
	l.size += int64(n)
	return ev
}

// turn moves the full file aside, over the oldest one, and starts a new
// one.
func (l *Log) turn() {
	_ = l.f.Close()
	for i := LogFiles - 1; i > 0; i-- {
		from := l.path
		if i > 1 {
			from = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		_ = os.Rename(from, fmt.Sprintf("%s.%d", l.path, i))
	}
	l.f, _ = os.OpenFile(l.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	l.size, l.seq = 0, 0
}

// Close closes the file of the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// followEvery is how often ReadLog looks for what was appended.
var followEvery = 200 * time.Millisecond

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
	var held os.FileInfo
	if f != nil {
		if held, err = f.Stat(); err != nil {
			return err
		}
	}
	for i := LogFiles - 1; i > 0; i-- {
		old, err := os.Open(fmt.Sprintf("%s.%d", path, i))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var rest []byte
		var rerr error
		if info, err := old.Stat(); err == nil && (held == nil || !os.SameFile(info, held)) {
			rerr = readEvents(old, &rest, fn)
		}
		_ = old.Close()
		if rerr != nil {
			return rerr
		}
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
			// the old one got last is read before the new one.
			if now, err := os.Stat(path); err == nil {
				if info, err := f.Stat(); err == nil && !os.SameFile(info, now) {
					if err := readEvents(f, &rest, fn); err != nil {
						return err
					}
					_ = f.Close()
					f = nil
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
		case <-time.After(followEvery):
		}
	}
}

// readEvents hands each whole line that f has from where it is to fn as an
// event; a line that is not one is skipped, and an unfinished one waits in
// rest for the rest of it.
func readEvents(f *os.File, rest *[]byte, fn func(Event)) error {
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		chunk, err := r.ReadSlice('\n')
		*rest = append(*rest, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err == nil {
			var ev Event
			if json.Unmarshal(bytes.TrimSpace(*rest), &ev) == nil {
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

// now is the time of an event: UTC, to the millisecond.
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
