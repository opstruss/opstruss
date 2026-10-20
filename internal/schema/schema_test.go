package schema

import (
	"slices"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		doc  map[string]any
		path string
		msg  string
	}{
		{name: "minimal", kind: Project, doc: map[string]any{"name": "app"}},
		{name: "extension keys", kind: Project, doc: map[string]any{"name": "app", "x-defaults": map[string]any{"timeout": "10m"}}},
		{name: "unknown key", kind: Project, doc: map[string]any{"name": "app", "unts": map[string]any{}}, path: "unts", msg: "unknown key"},
		{name: "bad name", kind: Project, doc: map[string]any{"name": "Web"}, path: "name", msg: `"Web" is not a name`},
		{name: "command type", kind: Project,
			doc:  map[string]any{"name": "app", "units": map[string]any{"web": map[string]any{"run": int64(5)}}},
			path: "units/web/run", msg: "expected array or string"},
		{name: "enum", kind: Project,
			doc:  map[string]any{"name": "app", "stacks": map[string]any{"dev": map[string]any{"approval": "maybe"}}},
			path: "stacks/dev/approval", msg: "must be one of 'ask', 'auto'"},
		{name: "branch that matched furthest", kind: Project,
			doc:  map[string]any{"name": "app", "uses": map[string]any{"d": map[string]any{"source": "./d", "wiht": map[string]any{}}}},
			path: "uses/d/wiht", msg: "unknown key"},
		{name: "include file keeps to its keys", kind: Include, doc: map[string]any{"name": "app"}, path: "name", msg: "unknown key"},
		{name: "plugin needs a name", kind: Plugin, doc: map[string]any{}, msg: `missing key "name"`},
		{name: "plugin cannot grant itself env", kind: Plugin, doc: map[string]any{"name": "p", "pass": []any{"HOME"}}, path: "pass", msg: "unknown key"},
		{name: "outputs tool", kind: Plugin, doc: map[string]any{"name": "p", "outputs": "tool"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probs, err := Validate(tt.kind, tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			if tt.msg == "" {
				if len(probs) > 0 {
					t.Fatalf("unexpected problems: %v", probs)
				}
				return
			}
			found := slices.ContainsFunc(probs, func(p Problem) bool {
				return strings.Join(p.Path, "/") == tt.path && strings.Contains(p.Message, tt.msg)
			})
			if !found {
				t.Fatalf("want %q at %q, got %v", tt.msg, tt.path, probs)
			}
		})
	}
}
