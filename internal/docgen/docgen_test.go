package main

import (
	"strings"
	"testing"

	"github.com/opstruss/opstruss/internal/cli"
	"github.com/spf13/cobra"
)

func TestSchemaPage(t *testing.T) {
	files := map[string]*object{}
	for name, data := range map[string]string{
		"project.json": `{"title": "opstruss.yaml", "description": "A project.", "type": "object",
			"properties": {
				"units": {"description": "The units.", "type": "object", "additionalProperties": {"$ref": "#/$defs/unit"}},
				"mode": {"enum": ["ask", "auto"], "description": "How it asks."}
			},
			"$defs": {
				"name": {"description": "A name.", "type": "string"},
				"unit": {"description": "A unit.", "type": "object", "required": ["run"],
					"properties": {
						"run": {"description": "The *program*, as <cmd>.", "type": "array", "items": {"type": "string"}},
						"needs": {"type": "array", "items": {"$ref": "#/$defs/name"}},
						"ready": {"type": "object", "properties": {"tcp": {"description": "A port.", "type": "string"}}}
					},
					"additionalProperties": {"$ref": "#/$defs/call"}},
				"call": {"type": "object"}
			}}`,
		"plugin.json": `{"title": "opstruss.plugin.yaml", "type": "object",
			"properties": {"name": {"$ref": "project.json#/$defs/name"}, "unit": {"$ref": "project.json#/$defs/unit"}}}`,
	} {
		doc, err := parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = doc.(*object)
	}
	project := schemaPage(files, "project.json")
	for _, want := range []string{
		"::: v-pre\n\n# opstruss.yaml\n\nA project.\n",
		"| `units` | map of [Unit](#unit) | The units. |\n| `mode` | one of `ask`, `auto` | How it asks. |\n",
		"## Unit {#unit}\n\nA unit.\n",
		"| `run` (required) | list of string | The \\*program\\*, as &lt;cmd&gt;. |\n",
		"| `needs` | list of string |  |\n",
		"| `ready` | [Unit › ready](#unit-ready) |",
		"Any other key is a call of a task by that name",
		"## Unit › ready {#unit-ready}\n",
	} {
		if !strings.Contains(project, want) {
			t.Errorf("project page lacks %q:\n%s", want, project)
		}
	}
	plugin := schemaPage(files, "plugin.json")
	if want := "| `unit` | [Unit](opstruss-yaml#unit) |"; !strings.Contains(plugin, want) {
		t.Errorf("plugin page lacks %q:\n%s", want, plugin)
	}
}

func TestCommandPages(t *testing.T) {
	root := &cobra.Command{Use: "opstruss", Short: "Run things"}
	root.PersistentFlags().String("stack", "", "the `name` of the stack")
	up := &cobra.Command{Use: "up [unit]...", Short: "Bring the units up", Annotations: map[string]string{"detachable": "true"}, Run: func(*cobra.Command, []string) {}}
	up.Flags().BoolP("yes", "y", false, "approve")
	up.Flags().Int("parallel", 10, "how many at once")
	hidden := &cobra.Command{Use: "secret", Hidden: true, Run: func(*cobra.Command, []string) {}}
	state := &cobra.Command{Use: "state", Short: "Read the state"}
	state.AddCommand(&cobra.Command{Use: "list", Short: "List the records", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(up, hidden, state)
	pages := commandPages(root, []cli.ShortFlag{{Name: "C", Arg: "dir", Usage: "run in `dir`", Global: true}, {Name: "d", Usage: "detach"}})
	if _, ok := pages["secret.md"]; ok {
		t.Error("a hidden command has a page")
	}
	for name, want := range map[string]string{
		"index.md":      "| `-C dir` | run in dir |\n| `--stack name` | the name of the stack |",
		"up.md":         "## Usage\n\n```text\nopstruss up [unit]... [flags]\n```",
		"state.md":      "| [`opstruss state list`](./state-list) | List the records |",
		"state-list.md": "## Global flags\n\n| Flag | Description |\n|---|---|\n| `-C dir` |",
	} {
		if !strings.Contains(pages[name], want) {
			t.Errorf("%s lacks %q:\n%s", name, want, pages[name])
		}
	}
	for _, want := range []string{"| `-d` | detach |", "| `-y`, `--yes` | approve |", "| `--parallel int` | how many at once (default 10) |"} {
		if !strings.Contains(pages["up.md"], want) {
			t.Errorf("up.md lacks %q:\n%s", want, pages["up.md"])
		}
	}
	if strings.Contains(pages["state-list.md"], "`-d`") {
		t.Error("-d is on a command that cannot run in the daemon")
	}
}
