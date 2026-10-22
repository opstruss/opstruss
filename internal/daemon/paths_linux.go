package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/opstruss/opstruss/internal/engine"
)

// paths sends the changes of the files that the watch tr names, as
// inotify reports them under the directory of its task and the fixed part
// of each absolute pattern, also in directories made later.
func (t *triggers) paths(ctx context.Context, tr *engine.Trigger, push func(map[string]any)) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.d.event("trigger", "error", tr.Task, "%s: %v", tr, err)
		return
	}
	defer func() { _ = w.Close() }()
	send := func(p, op string) {
		shown, ok := pathOf(tr, p)
		if !ok {
			return
		}
		push(map[string]any{"paths": []any{map[string]any{"path": shown, "op": op}}, "count": int64(1)})
	}
	// add watches root and the directories under it; in a directory made
	// after the watch started, the files that came before its watch did
	// are sent as created.
	add := func(root string, made bool) {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				if made {
					send(p, "create")
				}
				return nil
			}
			if name := d.Name(); p != root && (name == ".git" || name == ".opstruss") {
				return filepath.SkipDir
			}
			_ = w.Add(p)
			return nil
		})
	}
	for _, root := range pathRoots(tr) {
		add(root, false)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			t.d.event("diagnostic", "warn", tr.Task, "%s: %v", tr, err)
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					add(ev.Name, true)
					continue
				}
			}
			op := ""
			switch {
			case ev.Has(fsnotify.Create):
				op = "create"
			case ev.Has(fsnotify.Write):
				op = "write"
			case ev.Has(fsnotify.Remove):
				op = "remove"
			case ev.Has(fsnotify.Rename):
				op = "rename"
			default:
				continue
			}
			send(ev.Name, op)
		}
	}
}
