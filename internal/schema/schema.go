// Package schema holds the JSON Schemas of the files OpsTruss reads, the
// source of truth for their keys, and validates documents against them.
package schema

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed *.json
var files embed.FS

// Kind names a document that has a schema.
type Kind string

const (
	Project Kind = "project.json"
	Include Kind = "project.json#/$defs/includeFile"
	Plugin  Kind = "plugin.json"
	Record  Kind = "record.json"
	Meta    Kind = "meta.json"
	Run     Kind = "run.json"
	Event   Kind = "event.json"
)

const base = "https://schema.opstruss.invalid/"

// Problem is one place where a document does not match its schema.
type Problem struct {
	Path    []string
	Message string
}

var compiled = sync.OnceValues(func() (map[Kind]*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := files.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := c.AddResource(base+e.Name(), doc); err != nil {
			return nil, err
		}
	}
	out := map[Kind]*jsonschema.Schema{}
	for _, k := range []Kind{Project, Include, Plugin, Record, Meta, Run, Event} {
		s, err := c.Compile(base + string(k))
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
})

// Validate checks doc, a decoded YAML or JSON document, against the schema
// of k.
func Validate(k Kind, doc any) ([]Problem, error) {
	schemas, err := compiled()
	if err != nil {
		return nil, err
	}
	err = schemas[k].Validate(doc)
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	var out []Problem
	collect(ve, &out)
	slices.SortStableFunc(out, func(a, b Problem) int {
		return strings.Compare(strings.Join(a.Path, "\x00"), strings.Join(b.Path, "\x00"))
	})
	return slices.CompactFunc(out, func(a, b Problem) bool {
		return slices.Equal(a.Path, b.Path) && a.Message == b.Message
	}), nil
}

var printer = message.NewPrinter(language.English)

// collect reports the errors that say what is wrong: leaves, and for a
// oneOf the branch that matched the document furthest.
func collect(e *jsonschema.ValidationError, out *[]Problem) {
	if len(e.Causes) == 0 {
		*out = append(*out, leaf(e)...)
		return
	}
	switch e.ErrorKind.(type) {
	case *kind.OneOf, *kind.AnyOf:
		// Branches whose type does not match say nothing about the value;
		// of the others, the one that matched furthest explains it.
		var best *jsonschema.ValidationError
		depth := -1
		for _, c := range e.Causes {
			if mismatch(c, e.InstanceLocation) {
				continue
			}
			if d := deepest(c); d > depth {
				best, depth = c, d
			}
		}
		if best != nil {
			collect(best, out)
			return
		}
		*out = append(*out, Problem{Path: e.InstanceLocation, Message: "expected " + alternatives(e)})
		return
	}
	for _, c := range e.Causes {
		collect(c, out)
	}
}

// mismatch reports a branch that failed only on the type or constant of the
// value at loc.
func mismatch(e *jsonschema.ValidationError, loc []string) bool {
	if len(e.Causes) == 0 {
		switch e.ErrorKind.(type) {
		case *kind.Type, *kind.Const, *kind.Enum:
			return slices.Equal(e.InstanceLocation, loc)
		}
		return false
	}
	for _, c := range e.Causes {
		if !mismatch(c, loc) {
			return false
		}
	}
	return true
}

func deepest(e *jsonschema.ValidationError) int {
	d := len(e.InstanceLocation)
	for _, c := range e.Causes {
		d = max(d, deepest(c))
	}
	return d
}

// alternatives names what a oneOf accepts, from the type and const errors
// of its branches.
func alternatives(e *jsonschema.ValidationError) string {
	var want []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		switch k := e.ErrorKind.(type) {
		case *kind.Type:
			want = append(want, k.Want...)
		case *kind.Const:
			want = append(want, fmt.Sprintf("%v", k.Want))
		case *kind.Enum:
			for _, w := range k.Want {
				want = append(want, fmt.Sprintf("%v", w))
			}
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(e)
	slices.Sort(want)
	want = slices.Compact(want)
	if len(want) == 0 {
		return "a different value"
	}
	return strings.Join(want, " or ")
}

const namePattern = "^[a-z][a-z0-9_-]{0,62}$"

func leaf(e *jsonschema.ValidationError) []Problem {
	at := e.InstanceLocation
	switch k := e.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		var out []Problem
		for _, p := range k.Properties {
			out = append(out, Problem{Path: append(slices.Clone(at), p), Message: "unknown key"})
		}
		return out
	case *kind.Required:
		var out []Problem
		for _, m := range k.Missing {
			out = append(out, Problem{Path: at, Message: fmt.Sprintf("missing key %q", m)})
		}
		return out
	case *kind.Type:
		return []Problem{{Path: at, Message: fmt.Sprintf("expected %s, got %s", strings.Join(k.Want, " or "), k.Got)}}
	case *kind.Pattern:
		if k.Want == namePattern {
			return []Problem{{Path: at, Message: fmt.Sprintf("%q is not a name: lowercase letters, digits, - and _, starting with a letter, at most 63 characters", k.Got)}}
		}
		return []Problem{{Path: at, Message: fmt.Sprintf("%q does not match %s", k.Got, k.Want)}}
	case *kind.FalseSchema:
		return []Problem{{Path: at, Message: "not allowed here"}}
	}
	return []Problem{{Path: at, Message: e.ErrorKind.LocalizedString(printer)}}
}
