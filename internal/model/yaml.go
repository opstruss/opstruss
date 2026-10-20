package model

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// document is one file: its decoded value, and the node tree that knows
// where each value is written.
type document struct {
	file  string
	root  *yaml.Node
	value map[string]any
}

// parseYAML reads a file as YAML 1.2 core schema: on, off, yes, and no are
// strings, merge keys are allowed, and duplicate keys, explicit tags, and
// more than one document are errors.
func parseYAML(data []byte, file string) (*document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, Problems{{File: file, Line: 1, Column: 1, Message: "the file is empty"}}
		}
		return nil, Problems{{File: file, Line: 1, Column: 1, Message: err.Error()}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, Problems{{File: file, Line: extra.Line, Column: extra.Column, Message: "a file holds one document"}}
	}
	doc := root.Content[0]
	if err := checkTags(doc, file); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.MappingNode {
		return nil, Problems{{File: file, Line: doc.Line, Column: doc.Column, Message: "the file is a mapping of keys"}}
	}
	var v any
	if err := doc.Decode(&v); err != nil {
		return nil, Problems{{File: file, Line: 1, Column: 1, Message: err.Error()}}
	}
	value, err := normalize(v)
	if err != nil {
		return nil, Problems{{File: file, Line: 1, Column: 1, Message: err.Error()}}
	}
	return &document{file: file, root: doc, value: value.(map[string]any)}, nil
}

func checkTags(n *yaml.Node, file string) error {
	if n.Style&yaml.TaggedStyle != 0 {
		return Problems{{File: file, Line: n.Line, Column: n.Column,
			Message: fmt.Sprintf("tag %s is not allowed; write a value that starts with ! in quotes", n.Tag)}}
	}
	for _, c := range n.Content {
		if err := checkTags(c, file); err != nil {
			return err
		}
	}
	return nil
}

// normalize turns decoded YAML into the value types used everywhere:
// map[string]any, []any, string, bool, nil, and int64 for whole numbers in
// range, float64 otherwise, so 80 and 80.0 are the same value.
func normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[k] = n
		}
		return x, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("key %v is not a string; write it in quotes", k)
			}
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		for i, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	case int:
		return int64(x), nil
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), nil
		}
		return float64(x), nil
	case float64:
		return wholeNumber(x), nil
	}
	return v, nil
}

func wholeNumber(f float64) any {
	if f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
		return int64(f)
	}
	return f
}

// pos returns the line and column at which path is written: the key of a
// mapping entry, or the item of a list, or the nearest parent that exists.
func (d *document) pos(path []string) (int, int) {
	n := d.root
	line, col := n.Line, n.Column
	for _, p := range path {
		key, value := child(n, p)
		if value == nil {
			break
		}
		line, col = key.Line, key.Column
		n = value
	}
	return line, col
}

// child returns the key and value nodes of key in a mapping, or the item
// at index key twice in a list.
func child(n *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i], n.Content[i+1]
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "<<" {
				if k, v := child(n.Content[i+1], key); v != nil {
					return k, v
				}
			}
		}
	case yaml.SequenceNode:
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(n.Content) {
			return n.Content[i], n.Content[i]
		}
	}
	return nil, nil
}
