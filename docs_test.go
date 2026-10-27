package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/rogpeppe/go-internal/txtar"
)

// TestDocsExamples runs the projects in docs/examples, which the pages of
// the documentation include as they are: each one is valid, and one with a
// test.txtar also passes its script, with the files that it adds, such as
// fakes. A script skips what needs a tool that is not there, with
// [!exec:tofu] skip.
func TestDocsExamples(t *testing.T) {
	workRoot(t)
	ents, err := os.ReadDir(filepath.Join("docs", "examples"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	var files []string
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		dir := filepath.Join("docs", "examples", ent.Name())
		a := &txtar.Archive{Comment: []byte("env HOME=$WORK/home\nexec opstruss validate\nstdout '^valid$'\n")}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if rel == "test.txtar" {
				test := txtar.Parse(data)
				a.Comment = append(a.Comment, test.Comment...)
				a.Files = append(a.Files, test.Files...)
				return nil
			}
			a.Files = append(a.Files, txtar.File{Name: filepath.ToSlash(rel), Data: data})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(tmp, ent.Name()+".txtar")
		if err := os.WriteFile(file, txtar.Format(a), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	p := params("")
	p.Files = files
	testscript.Run(t, p)
}

// TestReadmeExample checks that the project in README.md is the example
// that TestDocsExamples runs.
func TestReadmeExample(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("docs", "examples", "hello", "opstruss.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(readme), "```yaml\n# opstruss.yaml\n")
	block, _, _ = strings.Cut(block, "```")
	if !ok || block != string(want) {
		t.Errorf("README.md does not show docs/examples/hello/opstruss.yaml as it is, under # opstruss.yaml:\n%s", block)
	}
}
