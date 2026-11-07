package model

import (
	"slices"
	"strings"
	"testing"
)

// TestSudo checks where sudo may be written, and that what a plugin asks
// to run as root is allowed by the unit, step, or declaration that uses it.
func TestSudo(t *testing.T) {
	plugin := "name: a\ntasks:\n  up:\n    run: [x]\n    sudo: true\n  destroy:\n    run: [x]\n    sudo: true\n  plan:\n    run: [x]\n" +
		"  build:\n    steps:\n      - run: [x]\n        sudo: true\n  secret:\n    run: [x]\n    sudo: true\n    outputs:\n      value: {}\n"
	files := func(project string, more ...string) map[string]string {
		m := map[string]string{"opstruss.yaml": "name: app\nuses:\n  a: ./plugins/a\n" + project,
			"plugins/a/opstruss.plugin.yaml": plugin,
			"components/c/opstruss.yaml":     "uses:\n  a: ../../plugins/a\nunits:\n  x:\n    uses: a\n  y:\n    run: [y]\n    sudo: true\n"}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	hook := func(name string) []string {
		return []string{"plugins/a/opstruss.plugin.yaml", "name: a\ntasks:\n  up:\n    run: [x]\n  " + name + ":\n    run: [x]\n    sudo: true\n"}
	}
	tests := []struct {
		files map[string]string
		want  string
	}{
		{files("units:\n  r:\n    run: [x]\n    destroy: [y]\n    sudo: true\n  p:\n    uses: a\n    sudo: true\n  b:\n    uses: a.build\n    sudo: true\n"), ""},
		{files("units:\n  c:\n    uses: ./components/c\n    sudo: true\n"), ""},
		{map[string]string{"opstruss.yaml": "name: app\nuses:\n  a:\n    source: ./plugins/a\n    sudo: true\nunits:\n  p:\n    uses: a\n",
			"plugins/a/opstruss.plugin.yaml": plugin}, ""},
		{files("tasks:\n  t:\n    sudo: true\n    steps:\n      - run: [x]\n        sudo: false\n      - block:\n          - a.build: {}\n      - command: {argv: [x]}\n      - kv: {key: k}\n  u:\n    a.build: {}\n    sudo: true\n"), ""},
		{files("units:\n  p:\n    uses: a\n"), "units.p: unit p: plugin a runs build, destroy, secret, and up with sudo; allow it with sudo: true on the unit"},
		{files("units:\n  b:\n    uses: a.build\n"), "units.b: unit b: task a.build runs with sudo; allow it with sudo: true on the unit"},
		{files("units:\n  c:\n    uses: ./components/c\n"), "units.x: unit c/x: plugin a runs build, destroy, secret, and up with sudo; allow it with sudo: true on the unit"},
		{files("tasks:\n  t:\n    steps:\n      - a.build: {}\n"), "tasks.t.steps[0]: task a.build runs with sudo; allow it with sudo: true on the step"},
		{files("tasks:\n  t:\n    sudo: true\n    steps:\n      - block:\n          - a.build: {}\n        sudo: false\n"), "tasks.t.steps[0].block[0]: task a.build runs with sudo; allow it with sudo: true on the step"},
		{files("tasks:\n  t:\n    a.build: {}\n"), "tasks.t: task a.build runs with sudo; allow it with sudo: true on the task"},
		{files("units:\n  s:\n    service: [x]\n    sudo: true\n"), "units.s.sudo: sudo is for the programs of a run; a service runs in the daemon"},
		{files("units:\n  k:\n    uses: kv\n    with: {key: k}\n    sudo: true\n"), "units.k.sudo: kv runs inside OpsTruss, not as a program; sudo is for programs"},
		{files("tasks:\n  t:\n    steps:\n      - task: u\n        sudo: true\n  u:\n    run: [x]\n"), "tasks.t.steps[0].sudo: a task: step passes data through with:; the task keeps its own sudo"},
		{files("tasks:\n  t:\n    steps:\n      - kv: {key: k}\n        sudo: true\n"), "tasks.t.steps[0].sudo: kv runs inside OpsTruss, not as a program; sudo is for programs"},
		{files("tasks:\n  t:\n    log: {message: hi}\n    sudo: true\n"), "tasks.t.sudo: log runs inside OpsTruss, not as a program; sudo is for programs"},
		{files("secrets:\n  s:\n    a.secret: {}\n"), "secrets.s.a.secret: a.secret runs with sudo, and a secret source runs as the operator"},
		{files("units:\n  w:\n    uses: a\n", hook("watch")...), "tasks.watch.sudo: watch is a hook, which runs as the operator; sudo is for the tasks that a run runs"},
		{files("units:\n  w:\n    uses: a\n", hook("exec")...), "tasks.exec.sudo: exec is a hook, which runs as the operator"},
		{files("units:\n  r:\n    run: [x]\n    sudo: \"yes\"\n"), "units.r.sudo: expected boolean, got string"},
		{files("tasks:\n  t:\n    steps:\n      - run: [x]\n        become: true\n"), "become is an Ansible keyword; OpsTruss writes sudo"},
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

	// sudo on a unit that uses a component allows what the units inside
	// ask for, and runs none of them as root that does not ask.
	p, err := Load(write(t, files("units:\n  c:\n    uses: ./components/c\n    sudo: true\n  r:\n    run: [x]\n")), "")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][2]bool{"c": {false, true}, "c/x": {false, true}, "c/y": {true, true}, "r": {false, false}} {
		n := p.Nodes[path]
		if got := [2]bool{n.SudoRun(), n.SudoAllowed()}; got != want {
			t.Errorf("%s: SudoRun, SudoAllowed = %v, want %v", path, got, want)
		}
	}
}
