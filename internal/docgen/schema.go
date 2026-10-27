package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// object is a JSON object with its keys in the order of the file, which
// is the order the reference shows them in.
type object struct {
	keys []string
	m    map[string]any
}

func (o *object) get(k string) any { return o.m[k] }

func (o *object) str(k string) string {
	s, _ := o.m[k].(string)
	return s
}

// parse reads JSON with the order of the keys of its objects.
func parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return value(dec)
}

func value(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		o := &object{m: map[string]any{}}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := value(dec)
			if err != nil {
				return nil, err
			}
			o.keys = append(o.keys, k.(string))
			o.m[k.(string)] = v
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		var a []any
		for dec.More() {
			v, err := value(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return tok, nil
}

// A section is the table of the keys of one object of a schema: the root,
// a definition, or an object written in place, such as ready of a unit.
type section struct {
	title, anchor, desc string
	node                *object
}

// schemas are the schema files of the reference, by name.
type schemas map[string]*object

// page documents one schema file.
type page struct {
	all      schemas
	file     string
	sections []*section
	at       map[*object]*section
}

// pages finds the sections of every file, so that a page links to the
// sections of another.
func (s schemas) pages() map[string]*page {
	pages := map[string]*page{}
	for name, root := range s {
		p := &page{all: s, file: name, at: map[*object]*section{}}
		p.find(root, root.str("title"), "", root.str("description"), true)
		if defs, ok := root.get("$defs").(*object); ok {
			for _, k := range defs.keys {
				def := defs.get(k).(*object)
				p.find(def, words(k), strings.ToLower(strings.ReplaceAll(words(k), " ", "-")), def.str("description"), false)
			}
		}
		pages[name] = p
	}
	return pages
}

// find adds a section for every object with properties in n that is not
// behind a $ref: n itself, or what its items, values, or alternatives
// are, under the same title, and the objects of its properties, under
// the title of the property.
func (p *page) find(n any, title, anchor, desc string, root bool) {
	o, ok := n.(*object)
	if !ok || o.get("$ref") != nil {
		return
	}
	desc = cmp.Or(o.str("description"), desc)
	if props, ok := o.get("properties").(*object); ok {
		s := &section{title: title, anchor: anchor, desc: desc, node: o}
		p.sections = append(p.sections, s)
		p.at[o] = s
		for _, k := range props.keys {
			sub, subAnchor := title+" › "+k, anchor+"-"+k
			if root {
				sub, subAnchor = k, k
			}
			p.find(props.get(k), sub, subAnchor, "", false)
		}
	}
	for _, k := range []string{"items", "additionalProperties"} {
		p.find(o.get(k), title, anchor, desc, root)
	}
	if alts, ok := o.get("oneOf").([]any); ok {
		for _, alt := range alts {
			p.find(alt, title, anchor, desc, root)
		}
	}
}

// resolve returns the file and the node that ref names, from file.
func (p *page) resolve(file, ref string) (string, *object) {
	name, frag, _ := strings.Cut(ref, "#")
	if name != "" {
		file = name
	}
	var n any = p.all[file]
	for _, part := range strings.Split(strings.Trim(frag, "/"), "/") {
		n = n.(*object).get(part)
	}
	return file, n.(*object)
}

// describe is the description of n, or of what its $ref names.
func (p *page) describe(file string, n any) string {
	o, ok := n.(*object)
	if !ok {
		return ""
	}
	if d := o.str("description"); d != "" {
		return d
	}
	if ref := o.str("$ref"); ref != "" {
		return p.describe(p.resolve(file, ref))
	}
	return ""
}

// typeOf says what n takes, with links to the sections of objects.
func (p *page) typeOf(pages map[string]*page, file string, n any) string {
	o, ok := n.(*object)
	if !ok {
		return "any"
	}
	if s := pages[file].at[o]; s != nil {
		link := "#" + s.anchor
		if file != p.file {
			link = pageFor[file] + link
		}
		return "[" + prose(s.title) + "](" + link + ")"
	}
	if ref := o.str("$ref"); ref != "" {
		f, target := p.resolve(file, ref)
		return p.typeOf(pages, f, target)
	}
	if c, ok := o.m["const"]; ok {
		return code(fmt.Sprint(c))
	}
	if enum, ok := o.get("enum").([]any); ok {
		var vs []string
		for _, v := range enum {
			vs = append(vs, code(fmt.Sprint(v)))
		}
		return "one of " + strings.Join(vs, ", ")
	}
	if alts, ok := o.get("oneOf").([]any); ok {
		var ts []string
		for _, alt := range alts {
			ts = append(ts, p.typeOf(pages, file, alt))
		}
		return strings.Join(ts, " or ")
	}
	switch t := o.get("type").(type) {
	case []any:
		var ts []string
		for _, v := range t {
			ts = append(ts, fmt.Sprint(v))
		}
		return strings.Join(ts, " or ")
	case string:
		switch {
		case t == "array" && o.get("items") != nil:
			return "list of " + p.typeOf(pages, file, o.get("items"))
		case t == "object" && o.get("additionalProperties") != nil:
			if _, ok := o.get("additionalProperties").(*object); ok {
				return "map of " + p.typeOf(pages, file, o.get("additionalProperties"))
			}
		}
		return t
	}
	return "any"
}

// render writes the page as markdown, in a v-pre container so that ${{ }}
// in the text stays as it is.
func (p *page) render(pages map[string]*page) string {
	var b strings.Builder
	root := p.all[p.file]
	fmt.Fprintf(&b, "---\ntitle: %s\n---\n\n%s::: v-pre\n\n", root.str("title"), header)
	for i, s := range p.sections {
		if i == 0 {
			fmt.Fprintf(&b, "# %s\n\n", prose(s.title))
		} else {
			fmt.Fprintf(&b, "## %s {#%s}\n\n", prose(s.title), s.anchor)
		}
		if s.desc != "" {
			fmt.Fprintf(&b, "%s\n\n", prose(s.desc))
		}
		if i == 0 {
			b.WriteString("Keys that start with x- are left for your own use.\n\n")
		}
		required := map[string]bool{}
		if req, ok := s.node.get("required").([]any); ok {
			for _, r := range req {
				required[r.(string)] = true
			}
		}
		b.WriteString("| Key | Type | Description |\n|---|---|---|\n")
		props := s.node.get("properties").(*object)
		for _, k := range props.keys {
			key := code(k)
			if required[k] {
				key += " (required)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", key, p.typeOf(pages, p.file, props.get(k)), prose(p.describe(p.file, props.get(k))))
		}
		if extra, ok := s.node.get("additionalProperties").(*object); ok && strings.HasSuffix(extra.str("$ref"), "/call") {
			b.WriteString("\nAny other key is a call of a task by that name, with its inputs as the value.\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(":::\n")
	return b.String()
}

// schemaPage is the reference page of the schema file name.
func schemaPage(all map[string]*object, name string) string {
	pages := schemas(all).pages()
	return pages[name].render(pages)
}

// words splits a name of a definition into words: encryptionKeys is
// Encryption keys, and mcp is MCP.
func words(name string) string {
	if name == "mcp" {
		return "MCP"
	}
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) {
			b.WriteByte(' ')
			r = unicode.ToLower(r)
		} else if i == 0 {
			r = unicode.ToUpper(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
