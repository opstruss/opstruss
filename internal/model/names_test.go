package model

import (
	"maps"
	"slices"
	"testing"
)

// TestNames keeps the names that a user cannot take as OpsTruss 1.0 has
// them. A name that a later version adds goes elsewhere and loses to a name
// of the user, with a warning; joining these lists would fail projects that
// 1.0 accepts.
func TestNames(t *testing.T) {
	for _, l := range []struct {
		name      string
		got, want []string
	}{
		{"BuiltinTasks", BuiltinTasks, []string{"command", "http", "read", "write", "set", "assert", "wait", "log", "kv"}},
		{"StandardTasks", StandardTasks, []string{"plan", "up", "destroy", "status", "logs"}},
		{"BuiltinFlags", BuiltinFlags, []string{"stack", "input", "json", "yes", "locked", "no-deps", "completion", "help", "event"}},
		{"Hooks", Hooks, []string{"watch", "exec", "migrate"}},
		{"Commands", Commands, []string{"init", "validate", "explain", "doctor", "list", "outputs", "stop", "mcp", "plugins",
			"unlock", "state", "runs", "run", "exec", "version", "help"}},
		{"keywords", slices.Sorted(maps.Keys(keywords)), []string{"always", "approval", "batch", "block", "changed_when",
			"concurrency", "continue_on_error", "debounce", "description", "dir", "env", "failed_when", "field", "for_each",
			"force", "id", "if", "inputs", "internal", "max_parallel", "name", "needs", "on", "outputs", "pass", "paths",
			"requires", "rescue", "retry", "run", "shell", "sops", "stdin", "steps", "stop", "stream", "task", "timeout",
			"unit", "units", "with", "yes"}},
	} {
		if !slices.Equal(l.got, l.want) {
			t.Errorf("%s is %q; 1.0 has %q, and a name added later loses to a name of the user instead", l.name, l.got, l.want)
		}
	}
}
