package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/opstruss/opstruss/internal/sources"
)

// RecordVersion and MetaVersion are the versions this OpsTruss writes. It
// reads every older version and migrates it when it writes, and it refuses
// a newer one instead of guessing.
const (
	RecordVersion = 1
	MetaVersion   = 1
)

// recordMigrations turns a record of version v into version v+1, one
// function per version.
var recordMigrations = map[int]func(map[string]any) error{}

// Record is the state of one unit at its address: enough to destroy the
// unit without its definition.
type Record struct {
	Version   int                          `json:"version"`
	ID        string                       `json:"id"`
	StateKey  string                       `json:"state_key,omitempty"`
	Address   string                       `json:"address"`
	Kind      string                       `json:"kind,omitempty"`
	Source    *Source                      `json:"source,omitempty"`
	Inputs    map[string]any               `json:"inputs,omitempty"`
	Destroy   *Destroy                     `json:"destroy,omitempty"`
	Needs     []string                     `json:"needs,omitempty"`
	Outputs   map[string]any               `json:"outputs,omitempty"`
	Sensitive map[string]map[string]string `json:"sensitive,omitempty"`
	Digest    string                       `json:"digest,omitempty"`
	Parts     map[string]string            `json:"parts,omitempty"` // a digest of each part of the definition, for plan to name what changed
	Run       string                       `json:"run,omitempty"`
	Updated   string                       `json:"updated,omitempty"`
	Pending   *Attempt                     `json:"pending,omitempty"`
	Abandoned []*Attempt                   `json:"abandoned,omitempty"`
	Tombstone *Tombstone                   `json:"tombstone,omitempty"`

	// Hidden names, by part, the values that are sensitive. A Store that
	// has the data key keeps them in Sensitive, encrypted, and gives them
	// back in their parts, in plain text, with their names here.
	Hidden map[string][]string `json:"-"`
}

// Source is where the program of a unit comes from.
type Source struct {
	Remote *sources.Identity `json:"remote,omitempty"`

	Uses   string `json:"uses,omitempty"`
	Commit string `json:"commit,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

// Destroy is what destroy runs, resolved: the program of a run unit or the
// plugin's task, with its context. Dir and Plugin are relative to their
// explicit source, or to the project root for older/local contracts.
// EnvFiles are env values that the program gets as files,
// and Pass holds the sha256 fingerprint of each variable passed from the
// operator.
type SourceLocation struct {
	Source sources.Identity `json:"source"`
	Root   string           `json:"root"`
}

type Destroy struct {
	Sources      []SourceLocation          `json:"sources,omitempty"`
	DirSource    *sources.Identity         `json:"dir_source,omitempty"`
	PluginSource *sources.Identity         `json:"plugin_source,omitempty"`
	Binaries     map[string]sources.Binary `json:"binaries,omitempty"`

	Run      any               `json:"run,omitempty"`
	Shell    string            `json:"shell,omitempty"`
	Dir      string            `json:"dir,omitempty"`
	Plugin   string            `json:"plugin,omitempty"` // the plugin's directory, relative to the project root
	Tool     bool              `json:"tool,omitempty"`   // the tool keeps its state in tool/, OPSTRUSS_STATE_DIR
	Env      map[string]string `json:"env,omitempty"`
	EnvFiles map[string]string `json:"env_files,omitempty"`
	Pass     map[string]string `json:"pass,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	Stop     *DestroyStop      `json:"stop,omitempty"`
}

// DestroyStop is how the program of destroy stops, or, for a service, how the
// service does: Run is its stop.run.
type DestroyStop struct {
	Signal string `json:"signal,omitempty"`
	Grace  string `json:"grace,omitempty"`
	Run    any    `json:"run,omitempty"`
}

// Attempt is an operation that started and has not finished: the pending
// mark, or an abandoned attempt, which keeps its own destroy contract.
type Attempt struct {
	Op        string         `json:"op"`
	Run       string         `json:"run"`
	Holder    Holder         `json:"holder"`
	Approval  string         `json:"approval,omitempty"`
	Digest    string         `json:"digest,omitempty"`
	Source    *Source        `json:"source,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
	Destroy   *Destroy       `json:"destroy,omitempty"`
	Needs     []string       `json:"needs,omitempty"`
	Outputs   map[string]any `json:"outputs,omitempty"`
	RawOutput string         `json:"raw_output,omitempty"`
	Peer      string         `json:"peer,omitempty"`
	Outcome   string         `json:"outcome,omitempty"` // failure or cancelled, once the attempt ended without succeeding
	Ended     string         `json:"ended,omitempty"`

	Sensitive map[string]map[string]string `json:"sensitive,omitempty"`
	Hidden    map[string][]string          `json:"-"` // as in Record
}

// Tombstone is what stays at an address that moved or was forgotten, so
// an old checkout cannot make a second unit there.
type Tombstone struct {
	MovedTo   string `json:"moved_to,omitempty"`
	Forgotten bool   `json:"forgotten,omitempty"`
	Run       string `json:"run"`
	Time      string `json:"time"`
}

// Meta is <stack>/_meta: the layout version of the stack, the project it
// belongs to, and, once a value needs it, the data key of the stack.
type Meta struct {
	Version int      `json:"version"`
	Project string   `json:"project"`
	Key     *DataKey `json:"key,omitempty"`
}

// DataKey is the data key in its envelopes: one that age opens, and one
// that the sops program does, each for the keys it was wrapped for.
type DataKey struct {
	Age  *AgeEnvelope  `json:"age,omitempty"`
	Sops *SopsEnvelope `json:"sops,omitempty"`
}

// AgeEnvelope is the data key encrypted with age, ASCII-armored, for the
// age recipients in To.
type AgeEnvelope struct {
	To   []string `json:"to"`
	Data string   `json:"data"`
}

// SopsEnvelope is the data key as the sops program encrypted it, for the
// keys in To, each a one-key map of encryption_keys, such as {kms: <arn>}.
type SopsEnvelope struct {
	To   []map[string]string `json:"to"`
	Data json.RawMessage     `json:"data"`
}

// NewerError is a version written by a newer OpsTruss.
type NewerError struct {
	What    string
	Version int
	Known   int
}

func (e *NewerError) Error() string {
	return fmt.Sprintf("%s has version %d, written by a newer OpsTruss; this one reads up to %d: upgrade OpsTruss", e.What, e.Version, e.Known)
}

// CheckVersion reads the version of data, under key, before anything else,
// since a newer version may have another shape: one newer than known is a
// *NewerError.
func CheckVersion(data []byte, key, what string, known int) error {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	var v int
	if raw, ok := head[key]; ok {
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: %s: %w", what, key, err)
		}
	}
	if v > known {
		return &NewerError{What: what, Version: v, Known: known}
	}
	return nil
}

// DecodeRecord reads a record of any version up to RecordVersion.
func DecodeRecord(data []byte, what string) (*Record, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if head.Version > RecordVersion {
		return nil, &NewerError{What: what, Version: head.Version, Known: RecordVersion}
	}
	if head.Version < 1 {
		return nil, fmt.Errorf("%s: no version", what)
	}
	if head.Version < RecordVersion {
		var m map[string]any
		if err := decodeJSON(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		for v := head.Version; v < RecordVersion; v++ {
			if err := recordMigrations[v](m); err != nil {
				return nil, fmt.Errorf("%s: migrating from version %d: %w", what, v, err)
			}
		}
		m["version"] = RecordVersion
		var err error
		if data, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	var r Record
	if err := decodeJSON(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	fixMap(r.Inputs)
	fixMap(r.Outputs)
	if r.Pending != nil {
		fixMap(r.Pending.Inputs)
		fixMap(r.Pending.Outputs)
	}
	for _, a := range r.Abandoned {
		fixMap(a.Inputs)
		fixMap(a.Outputs)
	}
	return &r, nil
}

// EncodeRecord writes a record at the current version, indented so it
// reads well from state pull.
func EncodeRecord(r *Record) ([]byte, error) {
	r.Version = RecordVersion
	c := *r
	c.Inputs, c.Outputs, c.Pending = floatsMap(r.Inputs), floatsMap(r.Outputs), floatsAttempt(r.Pending)
	c.Abandoned = nil
	for _, a := range r.Abandoned {
		c.Abandoned = append(c.Abandoned, floatsAttempt(a))
	}
	return encodeJSON(&c)
}

// floats copies v with each whole float64 written with a fraction, as
// 2.0, so that it reads back as a float, not an int: a value of type
// number stays one through state.
func floats(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			s := strconv.FormatFloat(x, 'g', -1, 64)
			if !strings.ContainsAny(s, ".eE") {
				s += ".0"
			}
			return json.Number(s)
		}
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = floats(e)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, e := range x {
			l[i] = floats(e)
		}
		return l
	}
	return v
}

func floatsMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return floats(m).(map[string]any)
}

func floatsAttempt(a *Attempt) *Attempt {
	if a == nil {
		return nil
	}
	c := *a
	c.Inputs, c.Outputs = floatsMap(a.Inputs), floatsMap(a.Outputs)
	return &c
}

// EncodeMeta writes _meta, indented so it reads well from state pull.
func EncodeMeta(m *Meta) ([]byte, error) { return encodeJSON(m) }

// DecodeMeta reads _meta.
func DecodeMeta(data []byte) (*Meta, error) {
	if err := CheckVersion(data, "version", "_meta", MetaVersion); err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("_meta: %w", err)
	}
	if m.Version < 1 {
		return nil, fmt.Errorf("_meta: no version")
	}
	return &m, nil
}

func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decodeJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// fixMap turns json.Number into the value types of the engine: int64 for
// an integer, float64 for a number written with a fraction or an exponent,
// as floats writes each float.
func fixMap(m map[string]any) {
	for k, v := range m {
		m[k] = fixNumbers(v)
	}
}

func fixNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		fixMap(x)
	case []any:
		for i, e := range x {
			x[i] = fixNumbers(e)
		}
	}
	return v
}
