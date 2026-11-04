// Command docgen writes the reference pages of the documentation from the
// JSON Schemas in internal/schema and the commands of opstruss. make
// docs-gen runs it as go run ./internal/docgen docs/reference, and CI
// checks that the pages it writes are the ones committed. With -links
// <file>, it writes the paths of the documentation that opstruss links to
// that file instead, one a line, which the docs workflow finds in each
// build.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/opstruss/opstruss/internal/cli"
)

// header marks a page that docgen writes.
const header = "<!-- Written by go run ./internal/docgen; edit the source, not this page. -->\n\n"

func main() {
	log.SetFlags(0)
	log.SetPrefix("docgen: ")
	links := flag.String("links", "", "write the paths of the documentation that opstruss links to this file")
	flag.Parse()
	if *links != "" {
		root, _ := cli.Reference()
		if err := os.WriteFile(*links, []byte(strings.Join(cli.LinkedPaths(root), "\n")+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
		return
	}
	dir := "docs/reference"
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	files := map[string]*object{}
	for _, name := range []string{"project.json", "plugin.json", "catalog.json"} {
		data, err := os.ReadFile(filepath.Join("internal", "schema", name))
		if err != nil {
			log.Fatal(err)
		}
		doc, err := parse(data)
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		files[name] = doc.(*object)
	}
	pages := map[string]string{
		"opstruss-yaml.md": schemaPage(files, "project.json"),
		"plugin-yaml.md":   schemaPage(files, "plugin.json"),
		"catalog-yaml.md":  schemaPage(files, "catalog.json"),
	}
	// Every page under cli/ comes from a command, so the pages of commands
	// that are gone go too.
	cliDir := filepath.Join(dir, "cli")
	if err := os.RemoveAll(cliDir); err != nil {
		log.Fatal(err)
	}
	root, short := cli.Reference()
	for name, page := range commandPages(root, short) {
		pages[filepath.Join("cli", name)] = page
	}
	for name, page := range pages {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

// pageFor is the page that a schema file is documented on.
var pageFor = map[string]string{"project.json": "opstruss-yaml", "plugin.json": "plugin-yaml", "catalog.json": "catalog-yaml"}

// prose is text of the schema or of help as markdown shows it as written:
// without emphasis, HTML, or the cells of a table.
func prose(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "<", "&lt;", ">", "&gt;", "|", `\|`).Replace(s)
}

// code is s as inline code.
func code(s string) string { return "`" + s + "`" }
