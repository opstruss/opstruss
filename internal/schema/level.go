package schema

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/message"
)

// Newest is the newest level of opstruss: that this OpsTruss reads.
const Newest = "1.1"

// Oldest is the level of a file without opstruss:.
const Oldest = "1.0"

// levelForm is the form of a level, MAJOR.MINOR.
var levelForm = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

func parseLevel(s string) (int, int) {
	maj, mn, _ := strings.Cut(s, ".")
	x, _ := strconv.Atoi(maj)
	y, _ := strconv.Atoi(mn)
	return x, y
}

// Newer reports whether level a is newer than level b.
func Newer(a, b string) bool {
	am, an := parseLevel(a)
	bm, bn := parseLevel(b)
	return am > bm || am == bm && an > bn
}

// LevelOf is the level that doc writes in opstruss:, or Oldest.
func LevelOf(doc any) string {
	if m, ok := doc.(map[string]any); ok {
		if s, ok := m["opstruss"].(string); ok && levelForm.MatchString(s) {
			x, y := parseLevel(s)
			return fmt.Sprintf("%d.%d", x, y)
		}
	}
	return Oldest
}

// levelled are the kinds whose keys a level gates: the files that people
// write, each with opstruss: of its own, or of its root for an include.
var levelled = map[Kind]bool{Project: true, Include: true, Plugin: true, Catalog: true}

// since is x-since on the schema of a key: the level that added it, which
// a file of an older level does not have, so it fails there with the level
// it needs rather than as an unknown key.
type since struct{ level, file string }

func (s since) Validate(ctx *jsonschema.ValidatorContext, _ any) {
	if Newer(s.level, s.file) {
		ctx.AddError(&kindSince{Want: s.level, Got: s.file})
	}
}

// kindSince is the error of a key that the level of its file does not
// have yet.
type kindSince struct{ Want, Got string }

func (k *kindSince) KeywordPath() []string { return []string{"x-since"} }

func (k *kindSince) LocalizedString(*message.Printer) string {
	return fmt.Sprintf("needs opstruss: %q or later in this file, which says %s", k.Want, k.Got)
}

// sinceVocabulary reads x-since for a file of level file.
func sinceVocabulary(file string) *jsonschema.Vocabulary {
	return &jsonschema.Vocabulary{URL: base + "vocab/since",
		Compile: func(_ *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
			v, ok := obj["x-since"]
			if !ok {
				return nil, nil
			}
			s, ok := v.(string)
			if !ok || !levelForm.MatchString(s) {
				return nil, fmt.Errorf("x-since %v is not a level such as \"1.1\"", v)
			}
			return since{level: s, file: file}, nil
		}}
}

// set is the schemas compiled for files of one level.
type set struct {
	once    sync.Once
	schemas map[Kind]*jsonschema.Schema
	c       *jsonschema.Compiler
	err     error
}

var sets sync.Map // by level

// compiledAt compiles the schemas for files of level once.
func compiledAt(level string) (map[Kind]*jsonschema.Schema, *jsonschema.Compiler, error) {
	v, _ := sets.LoadOrStore(level, &set{})
	s := v.(*set)
	s.once.Do(func() { s.schemas, s.c, s.err = compile(level) })
	return s.schemas, s.c, s.err
}
