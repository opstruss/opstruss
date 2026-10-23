// Package model reads opstruss.yaml, its include files, and
// opstruss.plugin.yaml into Go values, checked against the JSON Schema in
// internal/schema and the rules that a schema cannot express.
package model

import "github.com/opstruss/opstruss/internal/sources"

// File is opstruss.yaml: a project, or a component when used through uses:.
// Include files hold a subset of its keys.
type File struct {
	Name           string                    `json:"name,omitempty"`
	Description    string                    `json:"description,omitempty"`
	Level          string                    `json:"opstruss,omitempty"`
	Include        []string                  `json:"include,omitempty"`
	Uses           map[string]*Uses          `json:"uses,omitempty"`
	Inputs         map[string]*Input         `json:"inputs,omitempty"`
	Locals         map[string]any            `json:"locals,omitempty"`
	Outputs        map[string]*ProjectOutput `json:"outputs,omitempty"`
	Stacks         map[string]*Stack         `json:"stacks,omitempty"`
	Backend        *Backend                  `json:"backend,omitempty"`
	Secrets        map[string]*Secret        `json:"secrets,omitempty"`
	EncryptionKeys []*EncryptionKey          `json:"encryption_keys,omitempty"`
	Runtime        *Runtime                  `json:"runtime,omitempty"`
	MCP            *MCP                      `json:"mcp,omitempty"`
	Units          map[string]*Unit          `json:"units,omitempty"`
	Tasks          map[string]*Task          `json:"tasks,omitempty"`
	Moved          []*Move                   `json:"moved,omitempty"`
	Removed        []*Removed                `json:"removed,omitempty"`
}

// Uses declares a plugin, component, or built-in task under a name.
type Uses struct {
	Source string         `json:"source"`
	With   map[string]any `json:"with,omitempty"`
	Env    Env            `json:"env,omitempty"`
	Pass   []string       `json:"pass,omitempty"`
}

// Input is the schema of an input, also of an item or property of one.
type Input struct {
	Type        string            `json:"type,omitempty"`
	Description string            `json:"description,omitempty"`
	Required    bool              `json:"required,omitempty"`
	Default     any               `json:"default,omitempty"`
	Env         string            `json:"env,omitempty"`
	Enum        []any             `json:"enum,omitempty"`
	Pattern     string            `json:"pattern,omitempty"`
	Minimum     *float64          `json:"minimum,omitempty"`
	Maximum     *float64          `json:"maximum,omitempty"`
	Items       *Input            `json:"items,omitempty"`
	Properties  map[string]*Input `json:"properties,omitempty"`
	Sensitive   bool              `json:"sensitive,omitempty"`
	Literal     bool              `json:"literal,omitempty"`
	Args        bool              `json:"args,omitempty"`
}

// Output is an output of a task, step, run unit, or plugin.
type Output struct {
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	From        string `json:"from,omitempty"`
	Path        string `json:"path,omitempty"`
	Format      string `json:"format,omitempty"`
	Value       any    `json:"value,omitempty"`
}

// ProjectOutput is an output of a project or component: an expression, or
// a value with options.
type ProjectOutput struct {
	Value       any    `json:"value"`
	Description string `json:"description,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
}

// Stack is one instance of a project, with what it overrides.
type Stack struct {
	Description    string             `json:"description,omitempty"`
	With           map[string]any     `json:"with,omitempty"`
	Secrets        map[string]*Secret `json:"secrets,omitempty"`
	EncryptionKeys []*EncryptionKey   `json:"encryption_keys,omitempty"`
	Runtime        *Runtime           `json:"runtime,omitempty"`
	MCP            *MCP               `json:"mcp,omitempty"`
	Backend        *Backend           `json:"backend,omitempty"`
	Approval       string             `json:"approval,omitempty"`
	Include        []string           `json:"include,omitempty"`
}

// Backend is local, or a plugin backend.
type Backend struct {
	Local bool           `json:"-"`
	Uses  string         `json:"uses,omitempty"`
	With  map[string]any `json:"with,omitempty"`
	Pass  []string       `json:"pass,omitempty"`
}

// EncryptionKey is an entry of encryption_keys, which opens the data key
// of the stack: an age recipient, which OpsTruss opens itself, or one key
// that the sops program wraps it for, under its name in the creation rules
// of .sops.yaml, such as {kms: <arn>}.
type EncryptionKey struct {
	Age  string
	Sops map[string]string
}

// Secret is one secret source: sops, env, run, or a call of a plugin task.
type Secret struct {
	Sops    string   `json:"sops,omitempty"`
	Field   string   `json:"field,omitempty"`
	EnvName string   `json:"-"`
	Env     Env      `json:"-"`
	Run     *Command `json:"run,omitempty"`
	Pass    []string `json:"pass,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Shell   string   `json:"shell,omitempty"`
	Calls   Calls    `json:"-"`
}

// Runtime configures the OpsTruss that keeps running.
type Runtime struct {
	Listen string `json:"listen,omitempty"`
}

// MCP limits what an AI agent sees through opstruss mcp; Off is mcp: false.
type MCP struct {
	Off   bool     `json:"-"`
	Tasks []string `json:"tasks,omitempty"`
}

// Command is a program: a string runs through the shell, a list is argv.
type Command struct {
	Script string
	Argv   []string
}

// Env maps variable names to values.
type Env map[string]EnvValue

// EnvValue is text, or with File the text goes to a temporary file whose
// path is the variable.
type EnvValue struct {
	Text string
	File bool
}

// PathItem is a path or glob, or an exclude: of the patterns before it.
type PathItem struct {
	Pattern string
	Exclude bool
}

// Requires is a list of programs, or one expression that gives the list.
type Requires struct {
	Expr string
	List []string
}

// Stdin is what a program reads on its standard input.
type Stdin struct {
	Value  any    `json:"value"`
	Format string `json:"format,omitempty"`
}

// Stop is how a program is stopped.
type Stop struct {
	Signal string   `json:"signal,omitempty"`
	Grace  string   `json:"grace,omitempty"`
	Run    *Command `json:"run,omitempty"`
}

// Ready says when a service is ready.
type Ready struct {
	HTTP     string   `json:"http,omitempty"`
	TCP      string   `json:"tcp,omitempty"`
	Run      *Command `json:"run,omitempty"`
	Interval string   `json:"interval,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

// Retry repeats a unit, task, or step.
type Retry struct {
	Attempts int     `json:"attempts,omitempty"`
	Delay    string  `json:"delay,omitempty"`
	Backoff  float64 `json:"backoff,omitempty"`
	Until    any     `json:"until,omitempty"`
}

// Unit is a thing with state: exactly one of run, service, or uses.
type Unit struct {
	Description string             `json:"description,omitempty"`
	Run         *Command           `json:"run,omitempty"`
	Service     *Command           `json:"service,omitempty"`
	Uses        string             `json:"uses,omitempty"`
	With        map[string]any     `json:"with,omitempty"`
	Tasks       map[string]*Task   `json:"tasks,omitempty"`
	Requires    *Requires          `json:"requires,omitempty"`
	Shell       string             `json:"shell,omitempty"`
	Dir         string             `json:"dir,omitempty"`
	Env         Env                `json:"env,omitempty"`
	Pass        []string           `json:"pass,omitempty"`
	Stdin       *Stdin             `json:"stdin,omitempty"`
	Sources     []PathItem         `json:"sources,omitempty"`
	Generates   []string           `json:"generates,omitempty"`
	Revision    string             `json:"revision,omitempty"`
	Outputs     map[string]*Output `json:"outputs,omitempty"`
	Down        *Command           `json:"down,omitempty"`
	Restart     string             `json:"restart,omitempty"`
	Stop        *Stop              `json:"stop,omitempty"`
	Ready       *Ready             `json:"ready,omitempty"`
	Needs       []string           `json:"needs,omitempty"`
	If          any                `json:"if,omitempty"`
	ForEach     any                `json:"for_each,omitempty"`
	Approval    string             `json:"approval,omitempty"`
	Timeout     string             `json:"timeout,omitempty"`
	Protect     bool               `json:"protect,omitempty"`
	Retry       *Retry             `json:"retry,omitempty"`

	// Target is what uses: names, once the project is checked.
	Target *Target `json:"-"`
}

// Calls holds the keys of a task, step, secret source, or watch source that
// are not keywords: calls of a task by name, with their inputs. Exactly one
// is allowed where a call stands for the whole object.
type Calls map[string]map[string]any

// Task is a verb: exactly one of run, steps, or one call of a task.
type Task struct {
	Description     string             `json:"description,omitempty"`
	Inputs          map[string]*Input  `json:"inputs,omitempty"`
	Outputs         map[string]*Output `json:"outputs,omitempty"`
	Run             *Command           `json:"run,omitempty"`
	Steps           []*Step            `json:"steps,omitempty"`
	Shell           string             `json:"shell,omitempty"`
	Dir             string             `json:"dir,omitempty"`
	Env             Env                `json:"env,omitempty"`
	Pass            []string           `json:"pass,omitempty"`
	Stdin           *Stdin             `json:"stdin,omitempty"`
	Requires        *Requires          `json:"requires,omitempty"`
	Needs           []string           `json:"needs,omitempty"`
	If              any                `json:"if,omitempty"`
	ForEach         any                `json:"for_each,omitempty"`
	MaxParallel     int                `json:"max_parallel,omitempty"`
	On              *Triggers          `json:"on,omitempty"`
	Approval        string             `json:"approval,omitempty"`
	Concurrency     *Concurrency       `json:"concurrency,omitempty"`
	Timeout         string             `json:"timeout,omitempty"`
	Stop            *Stop              `json:"stop,omitempty"`
	Retry           *Retry             `json:"retry,omitempty"`
	FailedWhen      any                `json:"failed_when,omitempty"`
	ChangedWhen     any                `json:"changed_when,omitempty"`
	ContinueOnError bool               `json:"continue_on_error,omitempty"`
	Internal        bool               `json:"internal,omitempty"`
	Calls           Calls              `json:"-"`
}

// Concurrency limits runs of a task; Group gives one lock per value.
type Concurrency struct {
	Group string `json:"group,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

// Step is one step: exactly one of run, task, block, or one call of a task.
type Step struct {
	Name            string             `json:"name,omitempty"`
	ID              string             `json:"id,omitempty"`
	If              any                `json:"if,omitempty"`
	Run             *Command           `json:"run,omitempty"`
	Task            string             `json:"task,omitempty"`
	Block           []*Step            `json:"block,omitempty"`
	Rescue          []*Step            `json:"rescue,omitempty"`
	Always          []*Step            `json:"always,omitempty"`
	With            map[string]any     `json:"with,omitempty"`
	Units           []string           `json:"units,omitempty"`
	Yes             bool               `json:"yes,omitempty"`
	Force           bool               `json:"force,omitempty"`
	Env             Env                `json:"env,omitempty"`
	Pass            []string           `json:"pass,omitempty"`
	Shell           string             `json:"shell,omitempty"`
	Dir             string             `json:"dir,omitempty"`
	Stdin           *Stdin             `json:"stdin,omitempty"`
	Timeout         string             `json:"timeout,omitempty"`
	ForEach         any                `json:"for_each,omitempty"`
	Retry           *Retry             `json:"retry,omitempty"`
	FailedWhen      any                `json:"failed_when,omitempty"`
	ChangedWhen     any                `json:"changed_when,omitempty"`
	ContinueOnError bool               `json:"continue_on_error,omitempty"`
	Outputs         map[string]*Output `json:"outputs,omitempty"`
	Calls           Calls              `json:"-"`
}

// Triggers is on:; every key takes one trigger or a list.
type Triggers struct {
	Schedule OneOrMany[*Schedule] `json:"schedule,omitempty"`
	Every    OneOrMany[string]    `json:"every,omitempty"`
	Webhook  OneOrMany[*Webhook]  `json:"webhook,omitempty"`
	Watch    OneOrMany[*Watch]    `json:"watch,omitempty"`
	After    OneOrMany[*After]    `json:"after,omitempty"`
}

// OneOrMany is one value or a list of them.
type OneOrMany[T any] []T

type Schedule struct {
	Cron       string         `json:"cron"`
	Timezone   string         `json:"timezone,omitempty"`
	Persistent bool           `json:"persistent,omitempty"`
	If         any            `json:"if,omitempty"`
	With       map[string]any `json:"with,omitempty"`
}

type Webhook struct {
	Path      string         `json:"path"`
	Method    string         `json:"method,omitempty"`
	Secret    string         `json:"secret"`
	Signature *Signature     `json:"signature"`
	Respond   string         `json:"respond,omitempty"`
	If        any            `json:"if,omitempty"`
	With      map[string]any `json:"with,omitempty"`
}

// Signature is a named scheme, or a custom HMAC over the body.
type Signature struct {
	Scheme    string `json:"-"`
	Header    string `json:"header,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
}

// Watch has exactly one source: unit, paths, or one call of a task.
type Watch struct {
	Unit     string         `json:"unit,omitempty"`
	Paths    []PathItem     `json:"paths,omitempty"`
	Stream   string         `json:"stream,omitempty"`
	If       any            `json:"if,omitempty"`
	Batch    string         `json:"batch,omitempty"`
	Debounce string         `json:"debounce,omitempty"`
	With     map[string]any `json:"with,omitempty"`
	Calls    Calls          `json:"-"`
}

type After struct {
	Task    string         `json:"task"`
	Unit    string         `json:"unit,omitempty"`
	Outcome string         `json:"outcome,omitempty"`
	If      any            `json:"if,omitempty"`
	With    map[string]any `json:"with,omitempty"`
}

type Move struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Removed struct {
	From    string `json:"from"`
	Destroy bool   `json:"destroy"`
}

// Plugin is opstruss.plugin.yaml.
type Plugin struct {
	Name        string                        `json:"name"`
	Description string                        `json:"description,omitempty"`
	Level       string                        `json:"opstruss,omitempty"`
	Requires    []string                      `json:"requires,omitempty"`
	Env         []string                      `json:"env,omitempty"`
	State       string                        `json:"state,omitempty"`
	Inputs      map[string]*Input             `json:"inputs,omitempty"`
	Outputs     PluginOutputs                 `json:"outputs,omitempty"`
	Tasks       map[string]*Task              `json:"tasks,omitempty"`
	Bin         map[string]map[string]*Binary `json:"bin,omitempty"`
	Backend     *PluginBackend                `json:"backend,omitempty"`
}

// PluginOutputs is declared outputs, or Tool when the user's code in the
// tool names them.
type PluginOutputs struct {
	Tool     bool
	Declared map[string]*Output
}

type Binary = sources.Binary

type PluginBackend struct {
	Run *Command `json:"run"`
}
