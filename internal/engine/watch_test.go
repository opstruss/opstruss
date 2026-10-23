package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/opstruss/opstruss/internal/model"
)

func TestBatchLines(t *testing.T) {
	tr := &Trigger{Unit: "web", Stream: "both"}
	line := strings.Repeat("x", 64<<10)
	b := NewBatch(tr)
	if !b.Empty() {
		t.Fatal("a new batch is not empty")
	}
	for range 100 {
		b.Add(map[string]any{"line": line})
	}
	ev := b.Event()
	data, _ := json.Marshal(ev["lines"])
	lines := ev["lines"].([]any)
	// Each line takes its length and two quotes; the list adds brackets
	// and commas, which the bound leaves out.
	if size := len(data) - 1 - len(lines); size > MaxBytes {
		t.Fatalf("the lines take %d bytes, more than %d", size, MaxBytes)
	}
	if len(lines) != MaxBytes/(len(line)+2) || ev["truncated"] != int64(100-len(lines)) || ev["count"] != int64(100) {
		t.Fatalf("%d lines, truncated %v, count %v", len(lines), ev["truncated"], ev["count"])
	}
	if ev["unit"] != "web" || ev["stream"] != "both" {
		t.Fatalf("unit %v stream %v", ev["unit"], ev["stream"])
	}

	b = NewBatch(tr)
	for i := range MaxItems + 5 {
		b.Add(map[string]any{"line": fmt.Sprint(i)})
	}
	ev = b.Event()
	if n := len(ev["lines"].([]any)); n != MaxItems || ev["truncated"] != int64(5) || ev["count"] != int64(MaxItems+5) {
		t.Fatalf("%d lines, truncated %v, count %v", n, ev["truncated"], ev["count"])
	}
}

func TestBatchPaths(t *testing.T) {
	tr := &Trigger{Paths: []model.PathItem{{Pattern: "**"}}}
	change := func(path, op string) map[string]any {
		return map[string]any{"paths": []any{map[string]any{"path": path, "op": op}}, "count": int64(1)}
	}
	b := NewBatch(tr)
	b.Add(change("a", "create"))
	b.Add(change("a", "write"))
	b.Add(change("b", "write"))
	ev := b.Event()
	paths := ev["paths"].([]any)
	if len(paths) != 2 || paths[0].(map[string]any)["op"] != "create" || ev["count"] != int64(2) || ev["truncated"] != int64(0) {
		t.Fatalf("%v", ev)
	}

	// Once the list is full, a file in it stays merged, and one that is
	// not is counted each time it changes.
	b = NewBatch(tr)
	for i := range MaxItems {
		b.Add(change(fmt.Sprint(i), "write"))
	}
	b.Add(change("0", "write"))
	b.Add(change("new", "write"))
	b.Add(change("new", "write"))
	ev = b.Event()
	if n := len(ev["paths"].([]any)); n != MaxItems || ev["truncated"] != int64(2) || ev["count"] != int64(MaxItems+2) {
		t.Fatalf("%d paths, truncated %v, count %v", n, ev["truncated"], ev["count"])
	}
	if len(b.seen) != MaxItems {
		t.Fatalf("remembers %d paths", len(b.seen))
	}
}
