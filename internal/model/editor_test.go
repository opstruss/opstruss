package model

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// editorSchema compiles the schema that loading the project in dir wrote
// for editors.
func editorSchema(t *testing.T, dir string) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, EditorSchemaFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("editor.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("editor.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// editorAccepts checks that the schema for editors of the project in dir,
// which loads, takes each opstruss.yaml in it, so an editor marks nothing
// that validate takes.
func editorAccepts(t *testing.T, dir string) {
	t.Helper()
	s := editorSchema(t, dir)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != FileName {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := parseYAML(data, p)
		if err != nil {
			return nil // a file that no load reads
		}
		if err := s.Validate(doc.value); err != nil {
			t.Errorf("%s: the schema for editors refuses what loads:\n%s\n%v", p, data, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEditorSchema(t *testing.T) {
	project := `name: app
inputs:
  domain: {type: string}
uses:
  a: ./plugins/a
  fetch: opstruss.http
  site: ./components/site
  infra: ./vendor/infra
stacks:
  prod:
    with: {domain: example.com}
units:
  db:
    uses: a
    with: {file: x.yaml}
  web:
    uses: site
  dns:
    uses: infra.dns
  cache:
    uses: ./plugins/a
    with: {file: y.yaml}
tasks:
  deploy:
    inputs:
      env: {type: string, enum: [dev, prod]}
    on:
      schedule: {cron: "@daily", with: {env: prod}}
    run: [echo]
  release:
    steps:
      - task: deploy
        with: {env: dev}
`
	dir := write(t, map[string]string{
		"opstruss.yaml": project,
		"plugins/a/opstruss.plugin.yaml": "name: a\ndescription: The plugin a\ninputs:\n  file: {type: string, required: true}\n" +
			"tasks:\n  up:\n    run: [./up.sh]\n  build:\n    description: Build an image\n    inputs:\n      tag: {type: string}\n    run: [./build.sh]\n",
		"components/site/opstruss.yaml":     "name: site\ndescription: A site\ninputs:\n  replicas: {type: integer, minimum: 1}\n",
		"vendor/infra/dns/opstruss.yaml":    "name: dns\ninputs:\n  zone: {type: string}\n",
		"vendor/infra/mail/opstruss.yaml":   "name: mail\ninputs:\n  host: {type: string}\n",
		"vendor/infra/broken/opstruss.yaml": "name: [broken\n",
	})
	if got := problems(t, dir, ""); len(got) > 0 {
		t.Fatalf("unexpected problems:\n%s", strings.Join(got, "\n"))
	}
	s := editorSchema(t, dir)
	tests := []struct {
		old, new string
		ok       bool
	}{
		{"", "", true},
		// with: of a unit, by what it uses
		{"with: {file: x.yaml}", "with: {file: 42}", false},
		{"with: {file: x.yaml}", "with: {file: x, nope: 1}", false},
		{"with: {file: x.yaml}", `with: {file: "${{ inputs.domain }}"}`, true},
		{"with: {file: x.yaml}", "with: {file: null}", true},
		{"uses: site\n", "uses: site\n    with: {replicas: 0}\n", false},
		{"uses: site\n", "uses: site\n    with: {replicas: 2}\n", true},
		{"with: {file: y.yaml}", "with: {file: 1}", false},
		{"uses: infra.dns\n", "uses: infra.dns\n    with: {zone: 1}\n", false},
		{"uses: infra.dns\n", "uses: infra.mail\n    with: {host: 1}\n", false},
		{"uses: infra.dns\n", "uses: a.build\n    with: {file: x, tag: 1}\n", false},
		{"uses: infra.dns\n", "uses: unknown\n    with: {anything: 1}\n", true},
		// calls of tasks by name
		{"    run: [echo]\n", "    a.build: {file: x, tag: 1}\n", false},
		{"    run: [echo]\n", "    fetch: {url: x, method: 7}\n", false},
		{"    run: [echo]\n", "    opstruss.http: {url: x, nope: 1}\n", false},
		{"    run: [echo]\n", "    set: {anything: 1}\n", true},
		{"      - task: deploy\n", "      - http: {url: x}\n      - log: {msg: hi, level: loud}\n      - task: deploy\n", false},
		// task: of a step and the triggers of a task
		{"with: {env: dev}", "with: {env: staging}", false},
		{"with: {env: dev}", `with: {env: "${{ inputs.domain }}"}`, true},
		{"with: {env: prod}}", "with: {env: nope}}", false},
		{`schedule: {cron: "@daily", with: {env: prod}}`, `schedule: [{cron: "@daily", with: {env: nope}}]`, false},
		{`schedule: {cron: "@daily", with: {env: prod}}`, `schedule: "@daily"`, true},
		// with: of a stack and of a declaration
		{"with: {domain: example.com}", "with: {domain: example.com, nope: 1}", false},
		{"  a: ./plugins/a\n", "  a: {source: ./plugins/a, with: {tag: v1}}\n", true},
		{"  a: ./plugins/a\n", "  a: {source: ./plugins/a, with: {nope: 1}}\n", false},
	}
	for _, tt := range tests {
		text := strings.Replace(project, tt.old, tt.new, 1)
		if !strings.Contains(text, tt.new) {
			t.Fatalf("%q is not in the project", tt.old)
		}
		doc, err := parseYAML([]byte(text), "opstruss.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(doc.value); (err == nil) != tt.ok {
			t.Errorf("%q → %q: want ok %v, got %v", tt.old, tt.new, tt.ok, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, EditorSchemaFile))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseYAML(data, EditorSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	uses := pick(doc.value, "$defs", "unit", "properties", "uses", "anyOf", 0).(map[string]any)
	names := []string{}
	for _, n := range uses["enum"].([]any) {
		names = append(names, n.(string))
	}
	if want := []string{"a", "a.build", "deploy", "fetch", "infra.dns", "infra.mail", "release", "site"}; !slices.Equal(names, want) {
		t.Errorf("the names that uses: offers: want %v, got %v", want, names)
	}
	if got := uses["enumDescriptions"].([]any); got[0] != "The plugin a" || got[1] != "Build an image" {
		t.Errorf("descriptions of the names: %v", got)
	}

	// Each load writes the file again only when it changes.
	info, err := os.Stat(filepath.Join(dir, EditorSchemaFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := problems(t, dir, ""); len(got) > 0 {
		t.Fatal(got)
	}
	again, err := os.Stat(filepath.Join(dir, EditorSchemaFile))
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(info.ModTime()) || !os.SameFile(info, again) {
		t.Error("a load that changed nothing wrote the schema for editors again")
	}
}
