package model

import (
	"slices"
	"strings"
	"testing"
)

// TestTTY checks where tty may be written: on what runs a program that a run
// shows, and not on a service, a task: step, or the hooks whose output is
// not shown.
func TestTTY(t *testing.T) {
	plugin := "name: a\ntasks:\n  up:\n    run: [x]\n    tty: true\n  destroy:\n    run: [x]\n    tty: true\n  build:\n    run: [x]\n    tty: true\n"
	files := func(project string, more ...string) map[string]string {
		m := map[string]string{"opstruss.yaml": "name: app\nuses:\n  a: ./plugins/a\n" + project,
			"plugins/a/opstruss.plugin.yaml": plugin,
			"components/c/opstruss.yaml":     "units:\n  x:\n    run: [x]\n  y:\n    run: [y]\n    tty: false\n"}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	hook := func(name string) []string {
		return []string{"plugins/a/opstruss.plugin.yaml", "name: a\ntasks:\n  up:\n    run: [x]\n  " + name + ":\n    run: [x]\n    tty: true\n"}
	}
	tests := []struct {
		files map[string]string
		want  string
	}{
		{files("units:\n  r:\n    run: [x]\n    destroy: [y]\n    tty: true\n"), ""},
		{files("units:\n  p:\n    uses: a\n    tty: false\n  b:\n    uses: a.build\n    tty: false\n"), ""},
		{files("units:\n  c:\n    uses: ./components/c\n    tty: true\n"), ""},
		{files("units:\n  t:\n    uses: build\ntasks:\n  build:\n    tty: true\n    steps:\n      - run: [x]\n        tty: false\n      - block:\n          - run: [x]\n        tty: true\n      - a.build: {}\n        tty: false\n      - command: {argv: [x]}\n        tty: true\n"), ""},
		{files("units:\n  s:\n    service: [x]\n    tty: true\n"), "units.s.tty: tty is for programs that a run shows; a service runs in the daemon"},
		{files("tasks:\n  t:\n    steps:\n      - task: u\n        tty: true\n  u:\n    run: [x]\n"), "tasks.t.steps[0].tty: a task: step passes data through with:; the task keeps its own tty"},
		{files("units:\n  w:\n    uses: a\n", hook("watch")...), "tasks.watch.tty: the stdout of the watch hook is its events"},
		{files("units:\n  w:\n    uses: a\n", hook("exec")...), "tasks.exec.tty: the program of the exec hook has the terminal of opstruss exec already"},
		{files("units:\n  r:\n    run: [x]\n    tty: \"yes\"\n"), "units.r.tty: expected boolean, got string"},
		{files("tasks:\n  t:\n    run: [x]\n    tty: ${{ inputs.x }}\n"), "tasks.t.tty: expected boolean, got string"},
		{files("tasks:\n  t:\n    steps:\n      - run: [x]\n        tty: 1\n"), "tasks.t.steps[0].tty: expected boolean, got number"},
	}
	for _, tt := range tests {
		got := problems(t, write(t, tt.files), "")
		if tt.want == "" {
			if len(got) > 0 {
				t.Errorf("%q: unexpected problems:\n%s", tt.files["opstruss.yaml"], strings.Join(got, "\n"))
			}
			continue
		}
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, tt.want) }) {
			t.Errorf("%q: want %q, got:\n%s", tt.files["opstruss.yaml"], tt.want, strings.Join(got, "\n"))
		}
	}

	// A unit that uses a component gives tty to the units inside that do not
	// write it.
	p, err := Load(write(t, files("units:\n  c:\n    uses: ./components/c\n    tty: true\n")), "")
	if err != nil {
		t.Fatal(err)
	}
	if x := p.Nodes["c/x"].Def().TTY; x == nil || !*x {
		t.Errorf("c/x has tty %v; the component unit gives true", x)
	}
	if y := p.Nodes["c/y"].Def().TTY; y == nil || *y {
		t.Errorf("c/y has tty %v; its own false wins", y)
	}
}
