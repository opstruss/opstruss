package model

import (
	"fmt"
	"strconv"
	"strings"
)

// Problem is one thing wrong in a file, with where it is written.
type Problem struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	s := fmt.Sprintf("%s:%d:%d: ", p.File, p.Line, p.Column)
	if p.Path != "" {
		s += p.Path + ": "
	}
	return s + p.Message
}

// Problems is the error of files that do not hold a valid project.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// dotted writes a path the way it is read in a file: units.web.steps[0].run.
func dotted(path []string) string {
	var b strings.Builder
	for _, p := range path {
		if _, err := strconv.Atoi(p); err == nil {
			b.WriteString("[" + p + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(p)
	}
	return b.String()
}

// problem builds a Problem at path in doc.
func (d *document) problem(path []string, format string, args ...any) Problem {
	line, col := d.pos(path)
	return Problem{File: d.file, Line: line, Column: col, Path: dotted(path), Message: fmt.Sprintf(format, args...)}
}
