package record

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// Keep is how many runs of each task a stack keeps, and MaxRuns how much
// runs/<stack>/ takes, oldest dropped first; a task that runs every 30
// seconds would make 2,880 of them a day.
const (
	Keep    = 100
	MaxRuns = 1 << 30
)

// Interrupted marks the runs of stack under root that a crash left
// running: their .lock is free while their record says running. It
// returns their names, so their temporary files can go once no program of
// theirs is left.
func Interrupted(root, stack string) ([]string, error) {
	base := filepath.Join(root, "runs", stack)
	ents, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ent := range ents {
		dir := filepath.Join(base, ent.Name())
		rec, err := Read(dir)
		if err != nil || rec.Status != Running || held(filepath.Join(dir, ".lock")) {
			continue
		}
		rec.Status, rec.Outcome, rec.Reason = Finished, "cancelled", "interrupted"
		rec.Ended = time.Now().UTC().Format(time.RFC3339)
		if err := write(dir, rec); err != nil {
			return out, err
		}
		out = append(out, rec.Run)
	}
	return out, nil
}

// held reports whether a process holds the lock at p.
func held(p string) bool {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return errors.Is(syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
}

// Prune drops the oldest runs of stack under root beyond Keep per task,
// then beyond MaxRuns in all. It never drops a run still running, nor the
// last run that touched each unit, which logs reads.
func Prune(root, stack string) error {
	base := filepath.Join(root, "runs", stack)
	ents, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	type run struct {
		dir  string
		rec  *Record
		size int64
	}
	var runs []run
	for _, ent := range ents {
		dir := filepath.Join(base, ent.Name())
		rec, err := Read(dir)
		if err != nil {
			continue
		}
		runs = append(runs, run{dir: dir, rec: rec, size: size(dir)})
	}
	// Names start with the time, so they sort oldest first.
	slices.SortFunc(runs, func(a, b run) int { return compare(a.rec.Run, b.rec.Run) })
	keep := map[string]bool{}
	last := map[string]string{}
	for _, r := range runs {
		for u := range r.rec.Units {
			last[u] = r.rec.Run
		}
	}
	for _, run := range last {
		keep[run] = true
	}
	gone := map[string]bool{}
	var total int64
	for _, r := range runs {
		total += r.size
	}
	drop := func(r run) error {
		if keep[r.rec.Run] || gone[r.rec.Run] || r.rec.Status == Running {
			return nil
		}
		gone[r.rec.Run] = true
		total -= r.size
		return os.RemoveAll(r.dir)
	}
	perTask := map[string][]run{}
	for _, r := range runs {
		k := r.rec.Command + " " + r.rec.Task
		perTask[k] = append(perTask[k], r)
	}
	for _, k := range slices.Sorted(maps.Keys(perTask)) {
		list := perTask[k]
		for i := 0; i < len(list)-Keep; i++ {
			if err := drop(list[i]); err != nil {
				return err
			}
		}
	}
	for _, r := range runs {
		if total <= MaxRuns {
			break
		}
		if err := drop(r); err != nil {
			return err
		}
	}
	return nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func size(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
