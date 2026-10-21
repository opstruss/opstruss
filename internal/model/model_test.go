package model

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestSchemaMatchesStructs keeps the hand-written structs and the JSON
// Schema, the source of truth, on the same keys.
func TestSchemaMatchesStructs(t *testing.T) {
	project := readSchema(t, "project.json")
	plugin := readSchema(t, "plugin.json")
	defs := project["$defs"].(map[string]any)
	tests := []struct {
		name   string
		schema any
		typ    reflect.Type
		extra  []string // schema keys that a struct decodes by hand
	}{
		{"file", project, reflect.TypeFor[File](), nil},
		{"uses", pick(defs, "uses", "additionalProperties", "oneOf", 1), reflect.TypeFor[Uses](), nil},
		{"input", defs["input"], reflect.TypeFor[Input](), nil},
		{"output", defs["output"], reflect.TypeFor[Output](), nil},
		{"project output", pick(defs, "projectOutputs", "additionalProperties", "oneOf", 1), reflect.TypeFor[ProjectOutput](), nil},
		{"stack", defs["stack"], reflect.TypeFor[Stack](), nil},
		{"backend", pick(defs, "backend", "oneOf", 1), reflect.TypeFor[Backend](), nil},
		{"secret", defs["secret"], reflect.TypeFor[Secret](), []string{"env"}},
		{"runtime", defs["runtime"], reflect.TypeFor[Runtime](), nil},
		{"mcp", pick(defs, "mcp", "oneOf", 1), reflect.TypeFor[MCP](), nil},
		{"unit", defs["unit"], reflect.TypeFor[Unit](), nil},
		{"ready", pick(defs, "unit", "properties", "ready"), reflect.TypeFor[Ready](), nil},
		{"task", defs["task"], reflect.TypeFor[Task](), nil},
		{"concurrency", pick(defs, "task", "properties", "concurrency", "oneOf", 1), reflect.TypeFor[Concurrency](), nil},
		{"step", defs["step"], reflect.TypeFor[Step](), nil},
		{"triggers", defs["triggers"], reflect.TypeFor[Triggers](), nil},
		{"schedule", pick(defs, "schedule", "oneOf", 1), reflect.TypeFor[Schedule](), nil},
		{"webhook", defs["webhook"], reflect.TypeFor[Webhook](), nil},
		{"signature", pick(defs, "webhook", "properties", "signature", "oneOf", 1), reflect.TypeFor[Signature](), nil},
		{"watch", defs["watch"], reflect.TypeFor[Watch](), nil},
		{"after", defs["after"], reflect.TypeFor[After](), nil},
		{"stdin", defs["stdin"], reflect.TypeFor[Stdin](), nil},
		{"stop", defs["stop"], reflect.TypeFor[Stop](), nil},
		{"retry", defs["retry"], reflect.TypeFor[Retry](), nil},
		{"move", pick(defs, "moved", "items"), reflect.TypeFor[Move](), nil},
		{"removed", pick(defs, "removed", "items"), reflect.TypeFor[Removed](), nil},
		{"plugin", plugin, reflect.TypeFor[Plugin](), nil},
		{"binary", pick(plugin, "properties", "bin", "additionalProperties", "additionalProperties"), reflect.TypeFor[Binary](), nil},
		{"plugin backend", pick(plugin, "properties", "backend"), reflect.TypeFor[PluginBackend](), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props, _ := tt.schema.(map[string]any)["properties"].(map[string]any)
			want := slices.Sorted(maps.Keys(props))
			got := slices.Sorted(maps.Keys(jsonKeys(tt.typ)))
			got = slices.Sorted(slices.Values(append(got, tt.extra...)))
			if !slices.Equal(got, want) {
				t.Fatalf("struct keys %v, schema keys %v", got, want)
			}
		})
	}
}

func readSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "schema", name))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func pick(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// write makes a project from files and returns its directory.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// rewrite replaces the project's opstruss.yaml.
func rewrite(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// problems loads a project and returns its problems as text.
func problems(t *testing.T, dir, stack string) []string {
	t.Helper()
	_, err := Load(dir, stack)
	var probs Problems
	if !errors.As(err, &probs) {
		if err != nil {
			t.Fatal(err)
		}
		return nil
	}
	var out []string
	for _, p := range probs {
		out = append(out, p.String())
	}
	return out
}

func contains(t *testing.T, got []string, want string) {
	t.Helper()
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
		t.Fatalf("want a problem with %q, got:\n%s", want, strings.Join(got, "\n"))
	}
}

func TestLoad(t *testing.T) {
	dir := write(t, map[string]string{
		"opstruss.yaml": `
name: app
include: [tasks/*.yaml]
inputs:
  domain: {type: string, default: example.dev}
  replicas: {type: integer}
x-defaults: &defaults
  timeout: 10m
units:
  site:
    <<: *defaults
    run: [npm, run, build]
    on: not-a-trigger-here
`,
		"tasks/release.yaml": `
tasks:
  release:
    run: ./release.sh
`,
	})
	contains(t, problems(t, dir, ""), "opstruss.yaml:13:5: units.site.on: unknown key")

	rewrite(t, dir, `
name: app
include: [tasks/*.yaml]
on: x
ratio: 80.0
`)
	got := problems(t, dir, "")
	contains(t, got, "opstruss.yaml:4:1: on: unknown key")

	rewrite(t, dir, `
name: app
include: [tasks/*.yaml]
x-defaults: &defaults
  timeout: 10m
units:
  site:
    <<: *defaults
    run: [npm, run, build]
`)
	p, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.StackName != DefaultStack || p.Units["site"].Timeout != "10m" || p.Tasks["release"].Run.Script != "./release.sh" {
		t.Fatalf("unexpected project: stack %q, timeout %q, release %+v", p.StackName, p.Units["site"].Timeout, p.Tasks["release"].Run)
	}
}

func TestDialect(t *testing.T) {
	tests := map[string]string{
		"name: app\nunits:\n  a:\n    run: !!str x\n": "opstruss.yaml:4:10: tag !!str is not allowed",
		"name: app\nname: other\n":                    `mapping key "name" already defined`,
		"name: app\n---\nname: two\n":                 "a file holds one document",
		"- a\n- b\n":                                  "the file is a mapping of keys",
		"":                                            "the file is empty",
		"name: app\ninputs:\n  n:\n    default: {1: x}\n": "key 1 is not a string",
	}
	for content, want := range tests {
		dir := write(t, map[string]string{"opstruss.yaml": content})
		contains(t, problems(t, dir, ""), want)
	}
}

func TestInclude(t *testing.T) {
	dir := write(t, map[string]string{
		"opstruss.yaml": "name: app\ninclude: [a.yaml, b.yaml, none/*.yaml]\n",
		"a.yaml":        "units:\n  web:\n    run: [a]\n",
		"b.yaml":        "units:\n  web:\n    run: [b]\nname: no\n",
	})
	got := problems(t, dir, "")
	contains(t, got, "b.yaml:4:1: name: unknown key")
	contains(t, got, "opstruss.yaml:2:27: include[2]: none/*.yaml matches no file")

	dir = write(t, map[string]string{
		"opstruss.yaml": "name: app\ninclude: [a.yaml, b.yaml]\n",
		"a.yaml":        "units:\n  web:\n    run: [a]\n",
		"b.yaml":        "units:\n  web:\n    run: [b]\n",
	})
	contains(t, problems(t, dir, ""), "b.yaml:2:3: units.web: unit web is also defined in a.yaml")
}

func TestStacks(t *testing.T) {
	dir := write(t, map[string]string{
		"opstruss.yaml": `
name: app
inputs:
  domain: {type: string}
runtime: {listen: 127.0.0.1:7878}
stacks:
  dev:
    with: {domain: example.dev}
  prod:
    with: {domain: example.com, region: eu}
    runtime: {listen: 127.0.0.1:7879}
    include: [prod/*.yaml]
`,
		"prod/units.yaml": "units:\n  backup:\n    run: [backup]\n",
	})
	if _, err := Load(dir, ""); err == nil || !strings.Contains(err.Error(), "choose one with --stack") {
		t.Fatalf("want a stack choice error, got %v", err)
	}
	if _, err := Load(dir, "qa"); err == nil || !strings.Contains(err.Error(), "no stack qa") {
		t.Fatalf("want an unknown stack error, got %v", err)
	}
	contains(t, problems(t, dir, "prod"), "opstruss.yaml:10:33: stacks.prod.with.region: the project has no input region")

	rewrite(t, dir, `
name: app
inputs:
  domain: {type: string}
runtime: {listen: 127.0.0.1:7878}
stacks:
  dev:
    with: {domain: example.dev}
  prod:
    with: {domain: example.com}
    runtime: {listen: 127.0.0.1:7879}
    include: [prod/*.yaml]
`)
	dev, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Units["backup"] != nil || prod.Units["backup"] == nil {
		t.Fatal("a stack's include files belong to that stack only")
	}
	if dev.Runtime.Listen != "127.0.0.1:7878" || prod.Runtime.Listen != "127.0.0.1:7879" {
		t.Fatalf("runtime: dev %s, prod %s", dev.Runtime.Listen, prod.Runtime.Listen)
	}
}

func TestRules(t *testing.T) {
	tests := []struct{ yaml, want string }{
		{"units:\n  a:\n    run: [x]\n", "name: a project needs a name"},
		{"name: app\nopstruss: \"0.2\"\n", "this file is for OpsTruss 0.2 or newer"},
		{"name: app\nunits:\n  a:\n    run: [x]\n    service: [y]\n", "a unit has exactly one of run, service, or uses"},
		{"name: app\nunits:\n  a:\n    run: [x]\n    restart: always\n", "restart is for service units"},
		{"name: app\nunits:\n  a:\n    run: echo ${{ inputs.x }}\n", "${{ }} is not allowed in a script"},
		{"name: app\nunits:\n  a:\n    run: echo $${{ x }}\n", ""},
		{"name: app\nunits:\n  a:\n    run: [x]\n    needs: [b]\n", "there is no unit b"},
		{"name: app\ntasks:\n  up:\n    run: [x]\n", "up is a built-in command or a standard task"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    steps: [{run: [y]}]\n", "a task has exactly one of run, steps, or one call of a task"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - run: [x]\n        when: x == 1\n", "when is an Ansible keyword; OpsTruss writes if"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - run: [x]\n        with: {a: 1}\n", "with is for task: steps"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - task: u\n        env: {A: b}\n", "a task: step passes data through with:"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - id: a\n        run: [x]\n      - id: a\n        run: [y]\n", "step id a is used twice"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - rescue: [{run: [x]}]\n", "rescue and always go with block"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - run: [x]\n    outputs:\n      o: {type: string}\n", "an output of a task with steps has value:"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    needs: [n]\n  n:\n    inputs:\n      v: {type: string, required: true}\n    run: [y]\n", "task n needs input v"},
		{"name: app\ntasks:\n  t:\n    http: {url: x}\n", "calling a task by name is not supported yet"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    on: {every: 30s}\n", "triggers are not supported yet"},
		{"name: app\nuses:\n  d: ./plugins/d\n", "opstruss.yaml:3:3: uses.d: plugins/d does not exist"},
		{"name: app\nuses:\n  d: github.com/opstruss/plugins/d@v1\n", "git sources are not supported yet"},
		{"name: app\nuses:\n  http: ./plugins/d\n", "http is a built-in task"},
		{"name: app\nuses:\n  d: github.com/other/d@v1\n", "a git source is github.com/opstruss/<repo>[/<path>]@<ref>"},
		{"name: app\nuses:\n  d: github.com/opstruss/plugins/d\n", "a git source is github.com/opstruss/<repo>[/<path>]@<ref>"},
		{"name: app\ninputs:\n  token: {type: string, sensitive: true}\n", "sensitive values are not supported yet"},
		{"name: app\nsecrets:\n  s: {env: S}\n", "secrets: not supported yet"},
		{"name: app\nunits:\n  web:\n    run: [x]\nmoved:\n  - {from: web, to: site}\n", "web is still a unit"},
		{"name: app\nunits:\n  site:\n    run: [x]\nmoved:\n  - {from: old, to: new}\n", "there is no unit new"},
		{"name: app\nunits:\n  c:\n    run: [x]\nmoved:\n  - {from: a, to: b}\n  - {from: b, to: c}\n", ""},
		{"name: app\nunits:\n  a:\n    run: [x]\n    env: {OPSTRUSS_STACK: dev}\n", "opstruss.yaml:5:11: units.a.env.OPSTRUSS_STACK: OPSTRUSS_ and INPUT_ variables come from OpsTruss; env: cannot set them"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - run: [x]\n        pass: [AWS_*, INPUT_TOKEN]\n", "pass: never passes them"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    pass: ['*']\n", ""},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    inputs:\n      log-level: {type: string}\n      log_level: {type: string}\n", "inputs log-level and log_level are both INPUT_LOG_LEVEL"},
		{"name: app\nunits:\n  a:\n    run: [x]\n    stop: {signal: SIGFOO}\n", "unknown signal SIGFOO"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    stop: {signal: SIGTERM, grace: 5s}\n", ""},
	}
	for _, tt := range tests {
		dir := write(t, map[string]string{"opstruss.yaml": tt.yaml})
		got := problems(t, dir, "")
		if tt.want == "" {
			if len(got) > 0 {
				t.Errorf("%q: unexpected problems:\n%s", tt.yaml, strings.Join(got, "\n"))
			}
			continue
		}
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, tt.want) }) {
			t.Errorf("%q: want %q, got:\n%s", tt.yaml, tt.want, strings.Join(got, "\n"))
		}
	}
}

func TestResolveInputs(t *testing.T) {
	dir := write(t, map[string]string{"opstruss.yaml": `
name: app
inputs:
  flag: {type: integer}
  stacked: {type: string}
  env: {type: boolean, env: APP_DEBUG}
  fallback: {type: array, default: [a]}
  absent: {type: string}
stacks:
  dev:
    with: {stacked: from-stack, flag: 1}
`})
	p, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"APP_DEBUG": "true"}
	got, err := p.ResolveInputs(map[string]string{"flag": "3"}, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]InputValue{
		"flag":     {Value: int64(3), From: "--input"},
		"stacked":  {Value: "from-stack", From: "stack"},
		"env":      {Value: true, From: "env"},
		"fallback": {Value: []any{"a"}, From: "default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	if _, err := p.ResolveInputs(map[string]string{"flag": "x"}, nil); err == nil || !strings.Contains(err.Error(), `--input flag: "x" is not an integer`) {
		t.Fatalf("want a parse error, got %v", err)
	}
	if _, err := p.ResolveInputs(map[string]string{"nope": "1"}, nil); err == nil {
		t.Fatal("want an unknown input error")
	}
}

func TestReferences(t *testing.T) {
	tests := []struct{ yaml, want string }{
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ units.b.outputs.u }}\"]\n", "there is no unit b"},
		{"name: app\nunits:\n  b:\n    run: [y]\n    outputs: {u: {from: stdout}}\n  a:\n    run: [x, \"${{ units.b.outputs.nope }}\"]\n", "unit b has no output nope"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ units.b.outputs.u }}\"]\n  b:\n    run: [y, \"${{ units.a.outputs.u }}\"]\n    outputs: {u: {}}\n", "units a → b → a form a cycle"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ event.body }}\"]\n", "a unit cannot read event"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ opstruss.cwd }}\"]\n", "a unit cannot read opstruss.cwd"},
		{"name: app\nunits:\n  a:\n    run: [x]\n    if: opstruss.time > timestamp('2026-01-01T00:00:00Z')\n", "read only facts known at plan"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ each.key }}\"]\n", "each is only in something with for_each"},
		{"name: app\ntasks:\n  t:\n    steps:\n      - run: [x, \"${{ steps.b.outputs.stdout }}\"]\n      - id: b\n        run: [y]\n", "there is no step b before this one"},
		{"name: app\ntasks:\n  n:\n    run: [y]\n  t:\n    run: [x, \"${{ tasks.n.outputs.stdout }}\"]\n", "tasks.n is read through needs"},
		{"name: app\ntasks:\n  t:\n    run: [x]\n    if: outputs.exit_code == 0\n", "a task cannot read outputs"},
		{"name: app\ntasks:\n  t:\n    run: [x, \"${{ kv('k') }}\"]\n", "kv() is not supported yet"},
		{"name: app\nlocals:\n  l1: ${{ locals.l2 }}\n  l2: ${{ locals.l1 }}\n", "locals l1 → l2 → l1 form a cycle"},
		{"name: app\ntasks:\n  t:\n    steps: [{task: u}]\n  u:\n    run: [x]\n    needs: [t]\n", "tasks t → u → t form a cycle"},
		{"name: app\ntasks:\n  t:\n    steps: [{task: nope}]\n", "there is no task nope"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ inputs.nope }}\"]\n", "there is no input nope"},
		{"name: app\nunits:\n  a:\n    run: [x, \"${{ inputs. }}\"]\n", "Syntax error"},
		// What may be read, where.
		{"name: app\ninputs:\n  targets: {type: array, default: [docker, lxc]}\nunits:\n  b:\n    run: [y]\n    outputs: {u: {from: stdout}}\n  a:\n    run: [x, \"${{ each.value }}\", \"${{ units.b.outputs.u }}\"]\n    for_each: ${{ inputs.targets }}\n    if: each.key != 'vm' && opstruss.os == 'linux'\n", ""},
		{"name: app\ntasks:\n  n:\n    run: [y]\n  t:\n    needs: [n]\n    steps:\n      - id: a\n        run: [x, \"${{ tasks.n.outputs.stdout }}\"]\n        failed_when: outputs.exit_code > 1\n      - run: [x, \"${{ steps.a.outputs.stdout }}\", \"${{ event.time }}\"]\n", ""},
	}
	for _, tt := range tests {
		dir := write(t, map[string]string{"opstruss.yaml": tt.yaml})
		got := problems(t, dir, "")
		if tt.want == "" {
			if len(got) > 0 {
				t.Errorf("%q: unexpected problems:\n%s", tt.yaml, strings.Join(got, "\n"))
			}
			continue
		}
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, tt.want) }) {
			t.Errorf("%q: want %q, got:\n%s", tt.yaml, tt.want, strings.Join(got, "\n"))
		}
	}
}

func TestOrderAndLocals(t *testing.T) {
	dir := write(t, map[string]string{
		"opstruss.yaml": `
name: app
include: [more/units.yaml]
inputs:
  domain: {type: string, default: example.dev}
locals:
  prefix: ${{ opstruss.project }}-${{ stack }}
  host: ${{ locals.prefix }}.${{ inputs.domain }}
  later: ${{ kv('last') }}
units:
  web:
    run: [deploy]
    needs: [dns]
  db:
    run: [db]
    outputs: {url: {from: stdout}}
`,
		"more/units.yaml": "units:\n  dns:\n    run: [dns, \"${{ units.db.outputs.url }}\"]\nlocals:\n  ca: ${{ file('ca.pem') }}\n",
		"more/ca.pem":     "CERT",
	})
	_, err := Load(dir, "")
	if err == nil || !strings.Contains(err.Error(), "kv() is not supported yet") {
		t.Fatalf("want kv() refused, got %v", err)
	}
	rewrite(t, dir, strings.Replace(mustRead(t, filepath.Join(dir, "opstruss.yaml")), "  later: ${{ kv('last') }}\n", "", 1))
	p, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"db", "dns", "web"}; !slices.Equal(p.Order, want) {
		t.Fatalf("order %v, want %v", p.Order, want)
	}
	locals, err := p.EvalLocals(map[string]any{"domain": "example.dev"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"prefix": "app-default", "host": "app-default.example.dev", "ca": "CERT"}
	if !reflect.DeepEqual(locals, want) {
		t.Fatalf("locals %v, want %v", locals, want)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUses(t *testing.T) {
	plugin := "name: a\ninputs:\n  file: {type: string, required: true}\ntasks:\n  up:\n    run: [./up.sh]\n  exec:\n    run: [./exec.sh]\n  build:\n    inputs:\n      tag: {type: string}\n    run: [./build.sh]\n"
	files := func(project string, more ...string) map[string]string {
		m := map[string]string{"opstruss.yaml": "name: app\nuses:\n  a: ./plugins/a\n  fetch: opstruss.http\n" + project,
			"plugins/a/opstruss.plugin.yaml": plugin}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	gh := func(tasks string) []string {
		return []string{"plugins/gh/opstruss.plugin.yaml", "name: gh\ninputs:\n  repo: {type: string}\ntasks:\n" + tasks}
	}
	tests := []struct {
		files map[string]string
		want  string
	}{
		// what a unit uses
		{files("units:\n  db:\n    uses: a\n    with: {file: x.yaml}\n"), "units that use a plugin, task, or component are not supported yet"},
		{files("units:\n  db:\n    uses: ./plugins/a\n    with: {file: x.yaml}\n"), "units that use a plugin, task, or component are not supported yet"},
		{files("units:\n  img:\n    uses: a.build\n    with: {file: x, tag: v1}\n"), "units that use a plugin, task, or component are not supported yet"},
		{files("units:\n  img:\n    uses: build\ntasks:\n  build:\n    run: [x]\n"), "units that use a plugin, task, or component are not supported yet"},
		{files("units:\n  db:\n    uses: nope\n"), "opstruss.yaml:7:5: units.db.uses: there is no plugin, component, or task nope"},
		{files("units:\n  db:\n    uses: a.up\n"), "a.up runs through a unit that uses a; write uses: a"},
		{files("units:\n  db:\n    uses: a.x.y\n"), "names of three parts or more are reserved"},
		{files("units:\n  db:\n    uses: b.x\n"), "uses: declares no b"},
		{files("units:\n  db:\n    uses: a\n"), "a needs input file"},
		{files("units:\n  db:\n    uses: a\n    with: {file: x, nope: 1}\n"), "units.db.with.nope: a has no input nope"},
		{files("units:\n  db:\n    run: [x]\n    with: {file: x}\n"), "with is for units that use a plugin, task, or component"},
		{files("  gh: ./plugins/gh\nunits:\n  db:\n    uses: gh\n", gh("  comment:\n    run: [x]\n")...), "plugin gh has no up, so a unit uses one of its tasks, as uses: gh.<task>"},
		// calls of tasks by name
		{files("tasks:\n  t:\n    steps:\n      - fetch: {url: x}\n      - http: {url: x}\n      - opstruss.log: {msg: hi}\n      - a.build: {file: x}\n"), "calling a task by name is not supported yet"},
		{files("tasks:\n  t:\n    nope: {}\n"), "tasks.t.nope: there is no task nope"},
		{files("tasks:\n  t:\n    http: {bogus: 1}\n"), "tasks.t.http.bogus: http has no input bogus"},
		{files("tasks:\n  t:\n    http: {}\n"), "tasks.t.http: http needs input url"},
		{files("tasks:\n  t:\n    a.up: {}\n"), "a.up runs only through a unit: write uses: a in a unit, then opstruss up <unit> or task: up"},
		{files("tasks:\n  t:\n    a.exec: {}\n"), "a.exec is a hook that only opstruss exec calls"},
		{files("tasks:\n  t:\n    a: {}\n"), "a is not a task; call a task of it, as a.<task>"},
		{files("tasks:\n  t:\n    t2: {}\n  t2:\n    run: [x]\n"), "t2 is a task of this project; call it with task: t2"},
		{files("tasks:\n  t:\n    steps:\n      - http: {url: \"${{ inputs.nope }}\"}\n"), "there is no input nope"},
		// declarations
		{files("  b:\n    source: ./plugins/a\n    with: {tag: v1, nope: 1}\n"), "uses.b.with.nope: b has no input nope"},
		{files("", "plugins/a/components/build/opstruss.yaml", "name: build\n"), "a has a task and a component named build"},
		{files("  infra: ./vendor/infra\nunits:\n  dns:\n    uses: infra.dns\n", "vendor/infra/components/dns/opstruss.yaml", "name: dns\n"), "units that use a plugin, task, or component are not supported yet"},
		{files("  empty: ./vendor/empty\n", "vendor/empty/README.md", "x"), "vendor/empty has no opstruss.plugin.yaml, opstruss.yaml, or components/"},
		// the rules of a plugin file
		{files("  gh: ./plugins/gh\n", gh("  status:\n    run: [x]\n")...), "plugins/gh/opstruss.plugin.yaml:5:3: tasks.status: status runs only through a unit"},
		{files("  gh: ./plugins/gh\n", gh("  migrate:\n    run: [x]\n")...), "migrate is reserved"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    run: [x]\n    pass: [GH_TOKEN]\n")...), "pass: is not in opstruss.plugin.yaml"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    inputs:\n      repo: {type: string}\n    run: [x]\n")...), "repo is an input of the plugin already"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    needs: [d]\n    run: [x]\n  d:\n    run: [y]\n")...), "needs is for the tasks of a project"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    steps:\n      - task: d\n  d:\n    run: [y]\n")...), "a plugin task calls no task"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    approval: auto\n    run: [x]\n")...), "approval: auto is for the project to choose"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    run: [x, \"${{ units.db.outputs.x }}\"]\n")...), "a plugin task cannot read units"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    steps:\n      - a.build: {}\n")...), "a plugin task calls only built-in tasks"},
		{files("  gh: ./plugins/gh\n", gh("  c:\n    steps:\n      - http: {url: \"${{ inputs.repo }}\"}\n        failed_when: outputs.status == 404\n")...), ""},
		{files("  gh: ./plugins/gh\n", "plugins/gh/opstruss.plugin.yaml", "name: gh\ninputs:\n  token: {type: string, env: GH_TOKEN}\n"), "env: of an input is for the files of the project"},
		{files("  gh: ./plugins/gh\n", "plugins/gh/opstruss.plugin.yaml", "name: gh\ninputs:\n  force: {type: boolean}\ntasks:\n  up:\n    run: [x]\n"), "force is an argument of the tasks of a unit"},
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
}

func TestBuiltins(t *testing.T) {
	for _, name := range BuiltinTasks {
		if Builtin(name) == nil {
			t.Errorf("builtin.yaml has no task %s", name)
		}
	}
	if n := len(builtins()); n != len(BuiltinTasks) {
		t.Errorf("builtin.yaml has %d tasks, BuiltinTasks %d", n, len(BuiltinTasks))
	}
}
