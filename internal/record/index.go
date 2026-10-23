package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// indexVersion is the version of the lines of an index, in v; a line of
// another version is not read.
const indexVersion = 1

// index keeps what Prune read of the finished runs of a
// stack, in runs/<stack>/.index, so a new process reads one file rather
// than every record: a finished run does not change. The runs' own files
// are what counts: a run that the index lacks is read, and an entry whose
// directory went is dropped, so the index can be lost or behind, and an
// OpsTruss that does not know it leaves it alone. Lines are only appended,
// under the lock of the file, which is written anew once most of its lines
// are of runs that went.
type index struct {
	mu    sync.Mutex
	path  string
	runs  map[string]*entry // the finished runs, by name
	file  os.FileInfo       // the file read, to see when another process wrote it anew
	off   int64             // how much of it was read
	lines int               // the lines read
}

// entry is a line of an index: what Prune needs of a finished run.
type entry struct {
	V       int      `json:"v"`
	Run     string   `json:"run"`
	Command string   `json:"command"`
	Task    string   `json:"task,omitempty"`
	Units   []string `json:"units,omitempty"`
	Size    int64    `json:"size"`
}

// indexes are the indexes this process read, by the directory of the
// stack's runs, so a daemon reads only what other processes appended.
var indexes sync.Map

// scan lists the names of the runs of the stack in base, oldest first,
// and locks and loads its index, which keeps only the runs that are there;
// the caller unlocks it.
func scan(base string) ([]string, *index, error) {
	f, err := os.Open(base)
	if err != nil {
		return nil, nil, err
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, nil, err
	}
	// Names of runs start with the time, so they sort oldest first; .index
	// and its temporary files are not runs.
	names = slices.DeleteFunc(names, func(name string) bool { return strings.HasPrefix(name, ".") })
	slices.Sort(names)
	v, _ := indexes.LoadOrStore(base, &index{path: filepath.Join(base, ".index"), runs: map[string]*entry{}})
	ix := v.(*index)
	ix.mu.Lock()
	ix.load()
	known := 0
	for _, name := range names {
		if ix.runs[name] != nil {
			known++
		}
	}
	if known < len(ix.runs) {
		there := make(map[string]bool, len(names))
		for _, name := range names {
			there[name] = true
		}
		for name := range ix.runs {
			if !there[name] {
				delete(ix.runs, name)
			}
		}
	}
	return names, ix, nil
}

// entryOf is the entry of the finished run rec in dir.
func entryOf(dir string, rec *Record) *entry {
	return &entry{V: indexVersion, Run: rec.Run, Command: rec.Command, Task: rec.Task,
		Units: slices.Sorted(maps.Keys(rec.Units)), Size: size(dir)}
}

// load reads what was appended to the file since the last load, or all of
// it when another process wrote it anew; a line that a crash cut short
// waits for the newline that ends it.
func (ix *index) load() {
	f, err := os.Open(ix.path)
	if err != nil {
		ix.file = nil
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if ix.file == nil || !os.SameFile(info, ix.file) || info.Size() < ix.off {
		ix.off, ix.lines = 0, 0
	}
	ix.file = info
	if info.Size() == ix.off {
		return
	}
	buf := make([]byte, info.Size()-ix.off)
	n, err := f.ReadAt(buf, ix.off)
	if err != nil && n < len(buf) {
		return
	}
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return
	}
	for line := range bytes.SplitSeq(buf[:end], []byte{'\n'}) {
		ix.lines++
		var e entry
		if json.Unmarshal(line, &e) == nil && e.V == indexVersion && e.Run != "" {
			ix.runs[e.Run] = &e
		}
	}
	ix.off += int64(end) + 1
}

// add keeps the entries, and appends them to the file. A failed write
// only costs a later process the records it reads again.
func (ix *index) add(es []*entry) {
	if len(es) == 0 {
		return
	}
	var data []byte
	for _, e := range es {
		ix.runs[e.Run] = e
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		data = append(append(data, line...), '\n')
	}
	f, err := ix.lock()
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(data)
}

// compact writes the file anew with the runs it keeps, once most of its
// lines are of runs that went.
func (ix *index) compact() {
	if ix.lines < 2*len(ix.runs)+100 {
		return
	}
	f, err := ix.lock()
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	var data []byte
	for _, name := range slices.Sorted(maps.Keys(ix.runs)) {
		if line, err := json.Marshal(ix.runs[name]); err == nil {
			data = append(append(data, line...), '\n')
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(ix.path), ".index-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), ix.path) != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	// The next load reads the new file whole.
	ix.file = nil
}

// hold takes the lock of f, which others hold for moments; one held
// longer, as by a stopped process, is given up on, which only costs a
// later process the records it reads again.
func hold(f *os.File) error {
	for i := 0; ; i++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || i == 50 {
			return err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// lock opens the file to append to it, and takes its lock, which keeps
// lines of two processes apart; a file that another process wrote anew
// meanwhile is opened again.
func (ix *index) lock() (*os.File, error) {
	for {
		f, err := os.OpenFile(ix.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		if err := hold(f); err != nil {
			_ = f.Close()
			return nil, err
		}
		held, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		now, err := os.Stat(ix.path)
		if err == nil && os.SameFile(held, now) {
			return f, nil
		}
		_ = f.Close()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
}
