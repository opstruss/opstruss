package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// never sees half of it. A log that cannot be written loses the event, as
// the output of a program that fills the disk does.
func (l *Log) Emit(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	ev.V, ev.Seq, ev.Time = EventVersion, l.seq+1, now()
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if l.size > 0 && l.size+int64(len(data))+1 > logMax {
		l.turn()
		if l.f == nil {
			return
		}
		ev.Seq = 1
		data, _ = json.Marshal(ev)
	}
	l.seq = ev.Seq
	n, _ := l.f.Write(append(data, '\n'))
	l.size += int64(n)
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

// now is the time of an event: UTC, to the millisecond.
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
