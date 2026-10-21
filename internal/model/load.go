package model

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/opstruss/opstruss/internal/schema"
)

// FileName is the file that marks a project.
const FileName = "opstruss.yaml"

// Level is the newest level of opstruss: this OpsTruss reads.
const Level = "0.1"

// DefaultStack is the one stack of a project without stacks:.
const DefaultStack = "default"

// Project is a project read for one stack: opstruss.yaml merged with the
// include files of the project and of the stack, with the stack's
// overrides applied.
type Project struct {
	File
	Dir       string
	StackName string
	Stack     *Stack

	// Order is the units in an order where each comes after the units it
	// reads or needs, and Deps those units for each.
	Order []string
	Deps  map[string][]string

	// Packages are the names in uses: that stand for something.
	Packages map[string]*Package

	// doc is the merged document, for explain; origins says which file
	// and path wrote each entry, for messages.
	doc     map[string]any
	root    *document
	origins map[string]origin
}

type origin struct {
	doc  *document
	path []string
}

// Doc returns the merged document of the stack, as the files wrote it.
func (p *Project) Doc() map[string]any { return p.doc }

// Find looks for opstruss.yaml from dir upward and returns its directory.
func Find(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		_, err := os.Stat(filepath.Join(d, FileName))
		if err == nil {
			return d, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no %s in %s or a directory above it", FileName, dir)
		}
		d = parent
	}
}

// Stacks returns the stack names of the project in dir.
func Stacks(dir string) ([]string, error) {
	// Problems against the schema are for Load to report with the rest.
	root, _, err := readDocument(dir, FileName, schema.Project)
	if err != nil {
		return nil, err
	}
	return stackNames(root.value), nil
}

func stackNames(doc map[string]any) []string {
	stacks, _ := doc["stacks"].(map[string]any)
	if len(stacks) == 0 {
		return []string{DefaultStack}
	}
	return slices.Sorted(maps.Keys(stacks))
}

// Load reads the project in dir for one stack; an empty stack picks the
// only one.
func Load(dir, stack string) (*Project, error) {
	root, probs, err := readDocument(dir, FileName, schema.Project)
	if err != nil {
		return nil, err
	}
	names := stackNames(root.value)
	switch {
	case stack == "" && len(names) == 1:
		stack = names[0]
	case stack == "":
		return nil, fmt.Errorf("the project has stacks %s; choose one with --stack", strings.Join(names, ", "))
	case !slices.Contains(names, stack):
		return nil, fmt.Errorf("the project has no stack %s; it has %s", stack, strings.Join(names, ", "))
	}
	p := &Project{Dir: dir, StackName: stack, doc: map[string]any{}, root: root, origins: map[string]origin{}}
	for k, v := range root.value {
		if k == "include" || k == "stacks" || strings.HasPrefix(k, "x-") {
			continue
		}
		p.doc[k] = v
		p.originsOf(root, []string{k}, v)
	}
	probs = append(probs, p.include(root, []string{"include"})...)
	stackDoc, _ := mapAt(root.value, "stacks", stack)
	probs = append(probs, p.include(root, []string{"stacks", stack, "include"})...)
	p.overlay(root, stack, stackDoc)
	if len(probs) > 0 {
		return nil, probs
	}
	if err := decodeValue(p.doc, &p.File); err != nil {
		return nil, err
	}
	p.Stack = &Stack{}
	if stackDoc != nil {
		if err := decodeValue(stackDoc, p.Stack); err != nil {
			return nil, err
		}
	}
	if stacks, ok := root.value["stacks"]; ok {
		if err := decodeValue(map[string]any{"stacks": stacks}, &p.File); err != nil {
			return nil, err
		}
	}
	if probs := p.check(); len(probs) > 0 {
		return nil, probs
	}
	return p, nil
}

// Validate loads every stack of the project in dir and returns all
// problems found, once each.
func Validate(dir string) error {
	names, err := Stacks(dir)
	if err != nil {
		return err
	}
	var all Problems
	seen := map[string]bool{}
	for _, s := range names {
		_, err := Load(dir, s)
		var probs Problems
		if errors.As(err, &probs) {
			for _, pr := range probs {
				if !seen[pr.String()] {
					seen[pr.String()] = true
					all = append(all, pr)
				}
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	if len(all) > 0 {
		return all
	}
	return nil
}

// readDocument reads and parses a file, and returns how it differs from
// its schema; the document comes back with them, so the files it includes
// are still checked.
func readDocument(dir, rel string, k schema.Kind) (*document, Problems, error) {
	data, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return nil, nil, err
	}
	doc, err := parseYAML(data, rel)
	if err != nil {
		return nil, nil, err
	}
	found, err := schema.Validate(k, doc.value)
	if err != nil {
		return nil, nil, err
	}
	var probs Problems
	for _, f := range found {
		msg := f.Message
		// A key that is not a keyword reads as a call of a task, so an
		// Ansible keyword fails as one; say what OpsTruss writes instead.
		if n := len(f.Path); n >= 3 && (f.Path[n-3] == "steps" || f.Path[n-3] == "tasks" || f.Path[n-2] == "steps") {
			if hint, ok := ansibleHint(f.Path[n-1]); ok {
				msg = hint
			}
		}
		probs = append(probs, doc.problem(f.Path, "%s", msg))
	}
	return doc, probs, nil
}

func mapAt(doc map[string]any, path ...string) (map[string]any, bool) {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = m[p]
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

// originsOf records which file wrote each named entry of a section.
func (p *Project) originsOf(d *document, path []string, v any) {
	p.origins[strings.Join(path, ".")] = origin{doc: d, path: path}
	if m, ok := v.(map[string]any); ok && len(path) == 1 {
		for name := range m {
			p.origins[path[0]+"."+name] = origin{doc: d, path: append(slices.Clone(path), name)}
		}
	}
}

// mapSections and listSections are what include files may add.
var (
	mapSections  = []string{"uses", "inputs", "locals", "secrets", "units", "tasks"}
	listSections = []string{"moved", "removed"}
)

// include merges the files that the globs at path in d match. Names that
// two files define are an error, and nothing is merged deeply.
func (p *Project) include(d *document, path []string) Problems {
	globs, _ := valueAt(d.value, path).([]any)
	var probs Problems
	for i, g := range globs {
		pattern, ok := g.(string)
		if !ok {
			continue
		}
		at := append(slices.Clone(path), strconv.Itoa(i))
		matches, err := doublestar.Glob(os.DirFS(p.Dir), filepath.ToSlash(pattern))
		if err != nil {
			probs = append(probs, d.problem(at, "%v", err))
			continue
		}
		if len(matches) == 0 {
			probs = append(probs, d.problem(at, "%s matches no file", pattern))
			continue
		}
		slices.Sort(matches)
		for _, m := range matches {
			inc, ip, err := readDocument(p.Dir, filepath.FromSlash(m), schema.Include)
			var pp Problems
			switch {
			case errors.As(err, &pp):
				probs = append(probs, pp...)
			case err != nil:
				probs = append(probs, d.problem(at, "%v", err))
			case len(ip) > 0:
				probs = append(probs, ip...)
			default:
				probs = append(probs, p.merge(inc)...)
			}
		}
	}
	return probs
}

func (p *Project) merge(inc *document) Problems {
	var probs Problems
	for _, s := range mapSections {
		entries, _ := inc.value[s].(map[string]any)
		if len(entries) == 0 {
			continue
		}
		dst, _ := p.doc[s].(map[string]any)
		if dst == nil {
			dst = map[string]any{}
			p.doc[s] = dst
		}
		for name, v := range entries {
			if _, dup := dst[name]; dup {
				first := p.origins[s+"."+name]
				probs = append(probs, inc.problem([]string{s, name}, "%s %s is also defined in %s", strings.TrimSuffix(s, "s"), name, first.doc.file))
				continue
			}
			dst[name] = v
			p.origins[s+"."+name] = origin{doc: inc, path: []string{s, name}}
		}
	}
	for _, s := range listSections {
		items, _ := inc.value[s].([]any)
		if len(items) == 0 {
			continue
		}
		dst, _ := p.doc[s].([]any)
		for i, item := range items {
			p.origins[s+"."+strconv.Itoa(len(dst))] = origin{doc: inc, path: []string{s, strconv.Itoa(i)}}
			dst = append(dst, item)
		}
		p.doc[s] = dst
	}
	return probs
}

// overlay applies what a stack overrides: secrets by name, and runtime,
// mcp, and backend as a whole.
func (p *Project) overlay(root *document, stack string, sd map[string]any) {
	if sd == nil {
		return
	}
	at := func(k string) []string { return []string{"stacks", stack, k} }
	if secrets, ok := sd["secrets"].(map[string]any); ok {
		dst, _ := p.doc["secrets"].(map[string]any)
		if dst == nil {
			dst = map[string]any{}
			p.doc["secrets"] = dst
		}
		for name, v := range secrets {
			dst[name] = v
			p.origins["secrets."+name] = origin{doc: root, path: append(at("secrets"), name)}
		}
	}
	for _, k := range []string{"runtime", "mcp", "backend"} {
		if v, ok := sd[k]; ok {
			p.doc[k] = v
			p.origins[k] = origin{doc: root, path: at(k)}
		}
	}
}

func valueAt(doc map[string]any, path []string) any {
	var cur any = doc
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// problem builds a Problem for a path in the merged project, at the place
// of the file that wrote it.
func (p *Project) problem(path []string, format string, args ...any) Problem {
	for n := len(path); n > 0; n-- {
		if o, ok := p.origins[strings.Join(path[:n], ".")]; ok {
			full := append(slices.Clone(o.path), path[n:]...)
			pr := o.doc.problem(full, format, args...)
			pr.Path = dotted(path)
			return pr
		}
	}
	return p.root.problem(path, format, args...)
}
