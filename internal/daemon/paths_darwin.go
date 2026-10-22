package daemon

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/opstruss/opstruss/internal/engine"
)

// MaxPolled is how many files one watch of paths looks at on macOS, where
// it reads the stat of each every second, as watchexec --poll does.
const MaxPolled = 50000

type stamp struct {
	mod  time.Time
	size int64
}

// paths sends the changes of the files that the watch tr names, by the
// stat of each every second, under the directory of its task and the
// fixed part of each absolute pattern.
func (t *triggers) paths(ctx context.Context, tr *engine.Trigger, push func(map[string]any)) {
	roots := pathRoots(tr)
	scan := func() (map[string]stamp, bool) {
		seen := map[string]stamp{}
		for _, root := range roots {
			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					if name := d.Name(); p != root && (name == ".git" || name == ".opstruss") {
						return filepath.SkipDir
					}
					return nil
				}
				shown, ok := pathOf(tr, p)
				if !ok {
					return nil
				}
				if len(seen) >= MaxPolled {
					return filepath.SkipAll
				}
				if info, err := d.Info(); err == nil {
					seen[shown] = stamp{info.ModTime(), info.Size()}
				}
				return nil
			})
		}
		return seen, len(seen) < MaxPolled
	}
	last, ok := scan()
	if !ok {
		t.d.event("diagnostic", "error", tr.Task, "%s: its paths hold more than %d files; narrow them or add exclude:", tr, MaxPolled)
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now, _ := scan()
		for p, st := range now {
			old, had := last[p]
			switch {
			case !had:
				push(map[string]any{"paths": []any{map[string]any{"path": p, "op": "create"}}, "count": int64(1)})
			case old != st:
				push(map[string]any{"paths": []any{map[string]any{"path": p, "op": "write"}}, "count": int64(1)})
			}
		}
		for p := range last {
			if _, ok := now[p]; !ok {
				push(map[string]any{"paths": []any{map[string]any{"path": p, "op": "remove"}}, "count": int64(1)})
			}
		}
		last = now
	}
}
