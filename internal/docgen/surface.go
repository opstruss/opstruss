package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/cli"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/proc"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"
)

// surfaceFile is where make docs-gen writes the surface, from the root of
// the repository: in a directory of its own, since testscript reads every
// .txt file in testdata/compat as a script. A release freezes a copy of it
// beside it as <major>.<minor>.txt, and TestSurface fails when a line of a
// frozen copy is gone.
const surfaceFile = "testdata/compat/surface/main.txt"

// contracts are the schemas of the files and the data that a release
// keeps, by the name that their lines start with.
var contracts = map[string]string{
	"project": "project.json", "plugin": "plugin.json", "catalog": "catalog.json", "lock": "lock.json",
	"event": "event.json", "run": "run.json", "record": "record.json", "meta": "meta.json", "daemon": "daemon.json",
}

// surface lists the facts of the contract of this version, one a line and
// sorted: the keys of the schemas, the built-in tasks, the commands and
// their flags, what expressions offer, the names that a project cannot
// take, and the variables, exit codes, documents, and program contract
// that the reference lists. Each line is one fact, such as one type or one
// value of a key, so that what a later version may do, such as take
// another type or another value, only adds lines. root is the root of the
// repository.
func surface(root string) ([]string, error) {
	var lines []string
	w := &schemaWalk{dir: filepath.Join(root, "internal", "schema"), files: map[string]map[string]any{}}
	for short, file := range contracts {
		doc, err := w.load(file)
		if err != nil {
			return nil, err
		}
		w.prefix = "schema " + short
		w.walk(file, doc, "", nil)
	}
	lines = append(lines, w.lines...)

	builtins, err := builtinLines(filepath.Join(root, "internal", "model", "builtin.yaml"))
	if err != nil {
		return nil, err
	}
	lines = append(lines, builtins...)

	cmd, short := cli.Reference()
	lines = append(lines, commandLines(cmd, short)...)
	// What sudo runs as root is no command of the help, but sudoers names
	// it, so its name stays.
	lines = append(lines, "command opstruss "+proc.SudoCommand)

	exprs, err := expr.Surface()
	if err != nil {
		return nil, err
	}
	lines = append(lines, exprs...)
	lines = append(lines, model.Surface()...)

	docs, err := referenceLines(filepath.Join(root, "docs", "reference"))
	if err != nil {
		return nil, err
	}
	lines = append(lines, docs...)
	slices.Sort(lines)
	return slices.Compact(lines), nil
}

// schemaWalk writes the lines of the keys of a schema, under the paths a
// user writes, such as units.<name>.ready.interval, never under the names
// of definitions, so that a schema may be written another way without a
// line going.
type schemaWalk struct {
	dir    string
	files  map[string]map[string]any
	prefix string
	lines  []string
}

func (w *schemaWalk) load(file string) (map[string]any, error) {
	if doc, ok := w.files[file]; ok {
		return doc, nil
	}
	data, err := os.ReadFile(filepath.Join(w.dir, file))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	w.files[file] = doc
	return doc, nil
}

// resolve returns the file and the node that ref names from file, and the
// key that the walk marks it with.
func (w *schemaWalk) resolve(file, ref string) (string, map[string]any, string) {
	name, frag, _ := strings.Cut(ref, "#")
	if name != "" {
		file = name
	}
	doc, err := w.load(file)
	if err != nil {
		panic(err) // the schemas are in the binary too, and their tests read every $ref
	}
	var n any = doc
	for _, part := range strings.Split(strings.Trim(frag, "/"), "/") {
		if part != "" {
			n = n.(map[string]any)[part]
		}
	}
	return file, n.(map[string]any), file + "#" + frag
}

// walk writes the keys under n, at path. seen holds the definitions that
// the path goes through, so that a definition within itself, as the steps
// of rescue, stops there.
func (w *schemaWalk) walk(file string, n map[string]any, path string, seen []string) {
	if ref, ok := n["$ref"].(string); ok {
		f, target, key := w.resolve(file, ref)
		if !slices.Contains(seen, key) {
			w.walk(f, target, path, append(slices.Clip(seen), key))
		}
	}
	if props, ok := n["properties"].(map[string]any); ok {
		for k, v := range props {
			w.key(file, join(path, k), v, seen)
		}
	}
	if pats, ok := n["patternProperties"].(map[string]any); ok {
		for pat, v := range pats {
			seg := "<name>"
			if pat == "^x-" {
				seg = "x-*"
			}
			w.key(file, join(path, seg), v, seen)
		}
	}
	if ap, ok := n["additionalProperties"].(map[string]any); ok {
		w.key(file, join(path, "<name>"), ap, seen)
	}
	if items, ok := n["items"].(map[string]any); ok {
		w.key(file, path+"[]", items, seen)
	}
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		alts, _ := n[k].([]any)
		for _, alt := range alts {
			if alt, ok := alt.(map[string]any); ok {
				w.walk(file, alt, path, seen)
			}
		}
	}
	for _, k := range []string{"then", "else"} {
		if branch, ok := n[k].(map[string]any); ok {
			w.walk(file, branch, path, seen)
		}
	}
}

// key writes the lines of one key, v its schema: that it is there, each
// type and value that it takes, and its default, then the keys under it.
func (w *schemaWalk) key(file, path string, v any, seen []string) {
	id := w.prefix + " " + path
	w.lines = append(w.lines, id)
	node, ok := v.(map[string]any)
	if !ok {
		w.lines = append(w.lines, id+" type any")
		return
	}
	for _, fact := range w.takes(file, node, seen) {
		w.lines = append(w.lines, id+" "+fact)
	}
	if d, ok := w.defaultOf(file, node); ok {
		w.lines = append(w.lines, id+" default "+encode(d))
	}
	w.walk(file, node, path, seen)
}

// takes is each type and each value that n takes, as "type <type>" and
// "value <JSON>".
func (w *schemaWalk) takes(file string, n map[string]any, seen []string) []string {
	var out []string
	switch t := n["type"].(type) {
	case string:
		out = append(out, "type "+t)
	case []any:
		for _, x := range t {
			out = append(out, fmt.Sprint("type ", x))
		}
	}
	if c, ok := n["const"]; ok {
		out = append(out, "value "+encode(c))
	}
	if enum, ok := n["enum"].([]any); ok {
		for _, x := range enum {
			out = append(out, "value "+encode(x))
		}
	}
	if ref, ok := n["$ref"].(string); ok {
		f, target, key := w.resolve(file, ref)
		if !slices.Contains(seen, key) {
			out = append(out, w.takes(f, target, append(slices.Clip(seen), key))...)
		}
	}
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		alts, _ := n[k].([]any)
		for _, alt := range alts {
			if alt, ok := alt.(map[string]any); ok {
				out = append(out, w.takes(file, alt, seen)...)
			}
		}
	}
	if len(out) == 0 {
		switch {
		case n["properties"] != nil || n["additionalProperties"] != nil || n["patternProperties"] != nil:
			out = append(out, "type object")
		case n["items"] != nil:
			out = append(out, "type array")
		case n["$ref"] == nil && n["oneOf"] == nil && n["anyOf"] == nil && n["allOf"] == nil:
			out = append(out, "type any")
		}
	}
	return out
}

// defaultOf is the default of n, or of the definition that it names.
func (w *schemaWalk) defaultOf(file string, n map[string]any) (any, bool) {
	if d, ok := n["default"]; ok {
		return d, true
	}
	if ref, ok := n["$ref"].(string); ok {
		f, target, _ := w.resolve(file, ref)
		return w.defaultOf(f, target)
	}
	return nil, false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func encode(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// builtinLines are the lines of the built-in tasks of builtin.yaml: each
// task, and each input and output with its types, values, and default.
func builtinLines(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var tasks map[string]map[string]any
	if err := yaml.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	w := &schemaWalk{files: map[string]map[string]any{}}
	for name, task := range tasks {
		w.lines = append(w.lines, "builtin "+name)
		for _, side := range []string{"inputs", "outputs"} {
			fields, _ := task[side].(map[string]any)
			w.prefix = "builtin " + name + " " + strings.TrimSuffix(side, "s")
			for k, v := range fields {
				w.key("", k, v, nil)
			}
		}
	}
	return w.lines, nil
}

// commandLines are the lines of root and of every command under it that
// help shows: the command, and each of its flags with its type, its short
// form, and a default other than none.
func commandLines(root *cobra.Command, short []cli.ShortFlag) []string {
	var lines []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		path := c.CommandPath()
		lines = append(lines, "command "+path)
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" {
				return
			}
			id := "flag " + path + " --" + f.Name
			lines = append(lines, id, id+" type "+f.Value.Type())
			if f.Shorthand != "" {
				lines = append(lines, id+" short -"+f.Shorthand)
			}
			if f.DefValue != "" && f.DefValue != "[]" && (f.Value.Type() != "bool" || f.DefValue != "false") {
				lines = append(lines, id+" default "+f.DefValue)
			}
		})
		// -f is --follow, taken by hand as -C and -d are, and help names it
		// in the line of --follow rather than among the short flags.
		if c.LocalFlags().Lookup("follow") != nil {
			lines = append(lines, "flag "+path+" -f")
		}
		for _, s := range short {
			if s.For(c) {
				lines = append(lines, "flag "+path+" -"+s.Name)
			}
		}
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() {
				walk(sub)
			}
		}
	}
	walk(root)
	return lines
}

var (
	quoted   = regexp.MustCompile("`([^`]+)`")
	field    = regexp.MustCompile(`^[a-z][a-z_]*$`)
	variable = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(<[A-Z]+>)?$`)
)

// stateField is a field of the state file of the program contract, as
// pending.op.
var stateField = regexp.MustCompile(`^[a-z][a-z_]*(\[\])?(\.[a-z][a-z_]*)?$`)

// referenceLines are what the reference lists as tables, in dir: the
// variables that OpsTruss reads, its exit codes, the fields of the
// documents of --json, and the variables of the program contract.
func referenceLines(dir string) ([]string, error) {
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		return string(data), err
	}
	var lines []string
	env, err := read("environment.md")
	if err != nil {
		return nil, err
	}
	for _, row := range table(env, "") {
		for _, v := range quotes(row[0]) {
			// What OPSTRUSS_LOG prints is not kept, nor is the variable.
			if variable.MatchString(v) && v != "OPSTRUSS_LOG" {
				lines = append(lines, "env "+v)
			}
		}
	}
	codes, err := read("exit-codes-and-json.md")
	if err != nil {
		return nil, err
	}
	for _, row := range table(codes, "## Exit codes") {
		for c := range strings.SplitSeq(row[0], ",") {
			lines = append(lines, "exit "+strings.TrimSpace(c))
		}
	}
	for _, row := range table(codes, "## Documents") {
		commands := quotes(row[0])
		if len(commands) == 0 {
			commands = []string{"*"}
		}
		for _, c := range commands {
			for _, f := range quotes(row[1]) {
				if field.MatchString(f) {
					lines = append(lines, "json "+c+" "+f)
				}
			}
		}
	}
	contract, err := read("program-contract.md")
	if err != nil {
		return nil, err
	}
	for _, row := range tables(contract) {
		for _, v := range quotes(row[0]) {
			if variable.MatchString(v) {
				lines = append(lines, "contract "+v)
			}
		}
	}
	for _, row := range table(contract, "## The state file") {
		for _, f := range quotes(row[0]) {
			if stateField.MatchString(f) {
				lines = append(lines, "contract OPSTRUSS_STATE "+f)
			}
		}
	}
	return lines, nil
}

// table is the rows of the first table after the line head in page, or of
// the first table of page when head is "", each as its cells, without the
// header.
func table(page, head string) [][]string {
	if head != "" {
		i := strings.Index(page, "\n"+head+"\n")
		if i < 0 {
			return nil
		}
		page = page[i+1:]
	}
	if all := tablesOf(page); len(all) > 0 {
		return all[0]
	}
	return nil
}

// tables is the rows of every table of page.
func tables(page string) [][]string {
	var rows [][]string
	for _, t := range tablesOf(page) {
		rows = append(rows, t...)
	}
	return rows
}

// tablesOf is every table of page, each as its rows without the header.
func tablesOf(page string) [][][]string {
	var out [][][]string
	var cur [][]string
	header := 0
	for line := range strings.SplitSeq(page+"\n", "\n") {
		if !strings.HasPrefix(line, "|") {
			if cur != nil {
				out = append(out, cur)
			}
			cur, header = nil, 0
			continue
		}
		// The header and the line under it.
		if header < 2 {
			header++
			cur = [][]string{}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		cur = append(cur, cells)
	}
	return out
}

// quotes are the texts in backquotes in s.
func quotes(s string) []string {
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
