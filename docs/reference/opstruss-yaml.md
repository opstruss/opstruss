---
title: opstruss.yaml
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss.yaml

An OpsTruss project, or a component when used through uses:.

Keys that start with x- are left for your own use.

| Key | Type | Description |
|---|---|---|
| `name` | string | The name of the project: lowercase letters, digits, - and \_, starting with a letter. |
| `description` | string | What the project is for. |
| `opstruss` | string | The oldest OpsTruss version that reads this file, as MAJOR.MINOR. |
| `include` | list of string | Files merged into the project, as globs relative to this file. |
| `uses` | map of string or [Uses](#uses) | Plugins, components, and catalogs that the project uses, by the name it calls them. |
| `inputs` | map of [Input](#input) | Inputs of the project, which stacks, --input, or a prompt give. |
| `locals` | object | Named values computed from inputs and other locals, read as locals.&lt;name&gt;. |
| `outputs` | map of string or [Project outputs](#project-outputs) | Outputs of the project, which opstruss outputs prints and a project that uses it as a component reads. |
| `stacks` | map of [Stack](#stack) | The stacks of the project: copies of its units, each with its inputs and its own state. |
| `backend` | `local` or [Backend](#backend) | Where the state of the stacks is kept: local, in .opstruss/state/. |
| `secrets` | map of [Secret](#secret) | Secrets of the project, read as secrets.&lt;name&gt; and hidden in what OpsTruss writes. |
| `encryption_keys` | list of string or [Encryption keys](#encryption-keys) | What opens the data key of each stack, which encrypts sensitive values in the state. |
| `runtime` | [Runtime](#runtime) | Settings of the daemon of each stack. |
| `mcp` | `false` or [MCP](#mcp) | Reserved for opstruss mcp, which a later version brings: the tasks it offers to AI agents, or false for none. |
| `units` | map of [Unit](#unit) | The units of the project, which up brings up and destroy takes away. |
| `tasks` | map of [Task](#task) | The tasks of the project, each also a command of opstruss. |
| `moved` | list of [Moved](#moved) | Units that changed address, so that their records move rather than being destroyed. |
| `removed` | list of [Removed](#removed) | Units gone from the files, and whether destroy takes away what they made or their records are forgotten. |

## Include file {#include-file}

A file merged into a project through include:.

| Key | Type | Description |
|---|---|---|
| `uses` | map of string or [Uses](#uses) | What a project uses, by name: a source, or an object with source and what every use of it gets. |
| `inputs` | map of [Input](#input) | Inputs by name. |
| `locals` | object | Named values, each an expression, read as locals.&lt;name&gt;. |
| `secrets` | map of [Secret](#secret) | Secrets by name. |
| `units` | map of [Unit](#unit) | Units by name. |
| `tasks` | map of [Task](#task) | Tasks by name. |
| `moved` | list of [Moved](#moved) | Addresses that changed, from the old to the new. |
| `removed` | list of [Removed](#removed) | Units gone from the files. |

## Env {#env}

Variables set for programs: a value, or {file: &lt;value&gt;} to write the value to a file and set the variable to its path.

| Key | Type | Description |
|---|---|---|
| `file` (required) | string | The value, written to a file whose path the variable gets. |

## Stdin {#stdin}

What a program reads on its standard input.

| Key | Type | Description |
|---|---|---|
| `value` (required) | any | The value: text, or data that format writes. |
| `format` | one of `text`, `json`, `yaml`, `lines`, `dotenv` | How the value is written; text by default. |

## Paths {#paths}

Paths and globs relative to this file, read as written, without shell variables; {exclude: &lt;glob&gt;} leaves files out of those before it.

| Key | Type | Description |
|---|---|---|
| `exclude` (required) | string | A glob of files to leave out of the patterns before it. |

## Retry {#retry}

Running a program again when it fails.

| Key | Type | Description |
|---|---|---|
| `attempts` | integer | How many times the program runs at most, the first one included. |
| `delay` | string | The wait before the second attempt. |
| `backoff` | number | What each wait is multiplied by for the next one. |
| `until` | string or boolean | A condition on the outputs that ends the attempts when true. |

## Stop {#stop}

How a program is stopped, at cancel, timeout, or the end of a service.

| Key | Type | Description |
|---|---|---|
| `signal` | string | The signal that asks the program to stop; SIGINT by default, SIGTERM for a service. |
| `grace` | string | How long the program has after the signal before it is killed; 1m by default, 10s for a service. |
| `run` | string or list of string | A program that stops a service, instead of the signal. |

## Input {#input}

An input: its type, its default, and the checks of its value.

| Key | Type | Description |
|---|---|---|
| `type` | one of `string`, `number`, `integer`, `boolean`, `object`, `array` | The type of the value. |
| `description` | string | What the input is for, which help shows. |
| `required` | boolean | Whether the input must be given. |
| `default` | any | The value when none is given. |
| `env` | string | A variable of the environment that gives the value; only in the files of a project. |
| `enum` | array | The values allowed. |
| `pattern` | string | A regular expression that a string must match. |
| `minimum` | number | The smallest number allowed. |
| `maximum` | number | The largest number allowed. |
| `items` | [Input](#input) | What each item of an array is. |
| `properties` | map of [Input](#input) | The keys of an object, each one an input; other keys are refused. |
| `sensitive` | boolean | Hide the value in what OpsTruss writes, and keep it encrypted in the state. |
| `literal` | boolean | Refuse ${{ }} in the value. |
| `args` | boolean | Fill the array with the arguments after -- on the command line. |

## Output {#output}

An output of a program or a task, and where its value comes from.

| Key | Type | Description |
|---|---|---|
| `type` | one of `string`, `number`, `integer`, `boolean`, `object`, `array` | The type of the value. |
| `description` | string | What the output is. |
| `sensitive` | boolean | Hide the value in what OpsTruss writes, and keep it encrypted in the state. |
| `from` | one of `stdout`, `stderr`, `file`, `output` | Where the value comes from: stdout, stderr, the file at path, or output, the lines name=value written to $OPSTRUSS\_OUTPUT; output by default. |
| `path` | string | The file that from: file reads, relative to the directory of the program. |
| `format` | one of `text`, `json`, `yaml`, `lines`, `dotenv` | How the text is read: text, json, yaml, lines, or dotenv. |
| `value` | any | The value, as an expression of the steps of the task. |

## Project outputs {#project-outputs}

Outputs of a project by name: an expression, or an object with value.

| Key | Type | Description |
|---|---|---|
| `value` (required) | any | The value, as an expression of the outputs of units. |
| `description` | string | What the output is. |
| `sensitive` | boolean | Hide the value in what OpsTruss writes. |

## Uses {#uses}

What a project uses, by name: a source, or an object with source and what every use of it gets.

| Key | Type | Description |
|---|---|---|
| `source` (required) | string | Where it comes from: a path, github.com/&lt;owner&gt;/&lt;repo&gt;[/&lt;path&gt;]@&lt;ref&gt;, &lt;catalog&gt;.&lt;name&gt;, or opstruss.&lt;task&gt; for a built-in task. |
| `with` | object | Inputs given to every use of it, under those of the use. |
| `env` | map of string or [Env](#env) | Variables set for its programs, under those of the use. |
| `pass` | list of string | Variables of the environment that its programs get when it asks for them, by name or pattern; not \* alone, since the units that use it keep their values in their digest. |

## Stack {#stack}

A stack: a copy of the units of the project with its own inputs and state.

| Key | Type | Description |
|---|---|---|
| `description` | string | What the stack is for. |
| `with` | object | Values of the inputs of the project in this stack. |
| `secrets` | map of [Secret](#secret) | Secrets of this stack, over those of the project. |
| `encryption_keys` | list of string or [Encryption keys](#encryption-keys) | What opens the data key of this stack, instead of those of the project. |
| `runtime` | [Runtime](#runtime) | Settings of the daemon of this stack. |
| `mcp` | `false` or [MCP](#mcp) | Reserved for opstruss mcp: what it offers in this stack. |
| `backend` | `local` or [Backend](#backend) | Where the state of this stack is kept. |
| `approval` | one of `ask`, `auto` | ask to wait for approval of changes in this stack, auto to apply them without asking. |
| `include` | list of string | Files merged into the project in this stack only. |

## Backend {#backend}

Where the state of a stack is kept.

| Key | Type | Description |
|---|---|---|
| `uses` (required) | string | The plugin of the backend. |
| `with` | object | Inputs of the backend plugin. |
| `pass` | list of string | Variables of the environment that the backend plugin gets. |

## Secret {#secret}

One source: sops, env, run, or a call of a plugin task whose output is value.

| Key | Type | Description |
|---|---|---|
| `sops` | string | A file encrypted with sops, relative to this file; a dotenv, JSON, or YAML file is a map. |
| `field` | string | One field of the decrypted file, as a dotted path. |
| `env` | string or map of string or [Env](#env) | The variable of the environment that holds the secret, or, with run:, variables for its program. |
| `run` | string or list of string | A program whose standard output is the secret, such as op read. |
| `pass` | list of string | Variables of the environment that the program of run: gets. |
| `dir` | string | The directory the program of run: starts in, relative to this file. |
| `shell` | one of `sh`, `bash`, `python`, `node`, `pwsh` | The shell that runs run: when it is a script. |

Any other key is a call of a task by that name, with its inputs as the value.

## Encryption keys {#encryption-keys}

What opens the data key of each stack: age recipients (age1...), opened by OpsTruss, and keys that the sops program wraps it for.

| Key | Type | Description |
|---|---|---|
| `kms` | string | An AWS KMS key ARN. |
| `gcp_kms` | string | A Google Cloud KMS key resource ID. |
| `azure_keyvault` | string | An Azure Key Vault key URL. |
| `hc_vault_transit_uri` | string | A HashiCorp Vault transit key URI. |
| `pgp` | string | A PGP key fingerprint. |

## Runtime {#runtime}

Settings of the daemon of a stack.

| Key | Type | Description |
|---|---|---|
| `listen` | string | The address where the daemon serves webhooks, as host:port; on: webhook needs it. |

## MCP {#mcp}

What opstruss mcp offers, or false for nothing.

| Key | Type | Description |
|---|---|---|
| `tasks` | list of string | The tasks that opstruss mcp offers as tools. |

## Unit {#unit}

A unit: something that up brings up and destroy takes away, with exactly one of run, service, or uses.

| Key | Type | Description |
|---|---|---|
| `description` | string | What the unit is. |
| `run` | string or list of string | The program that brings the unit up. |
| `service` | string or list of string | A program that keeps running, which the daemon of the stack supervises. |
| `uses` | string | The plugin, task, or component that the unit uses. |
| `with` | object | Inputs of what the unit uses. |
| `tasks` | map of [Task](#task) | Tasks of the unit, run as opstruss &lt;task&gt; [unit]... |
| `requires` | string or list of string | Programs that must be in PATH before the unit runs. |
| `shell` | one of `sh`, `bash`, `python`, `node`, `pwsh` | The shell that runs a script; sh by default. |
| `dir` | string | The directory that the programs start in, relative to this file. |
| `env` | map of string or [Env](#env) | Variables set for the programs of the unit. |
| `pass` | list of string | Variables of the environment that the programs of the unit get; not \* alone, since the unit keeps their values in its digest and destroy context. |
| `stdin` | [Stdin](#stdin) | What the program reads on its standard input. |
| `sources` | list of string or [Paths](#paths) | Files whose content is part of the definition, so a change to one runs the unit again. |
| `generates` | list of string | Files that the unit makes, relative to this file and read as written; a missing one runs the unit again. |
| `revision` | string | A value whose change runs the unit again. |
| `outputs` | map of [Output](#output) | Outputs of the program, which other units read as units.&lt;name&gt;.outputs.&lt;output&gt;. |
| `destroy` | string or list of string | The program that takes the unit away, which its record keeps. |
| `restart` | one of `no`, `on-failure`, `always` | When the daemon starts the service again after it exits: no, on-failure, or always; on-failure by default. |
| `stop` | [Stop](#stop) | How the program is stopped. |
| `ready` | [Unit › ready](#unit-ready) | When the service counts as started: a URL, a port, or a program; up waits for it. |
| `needs` | list of string | Units that come before this one, besides those its expressions read. |
| `if` | string or boolean | A condition that decides whether the unit is in the stack; one that leaves it is destroyed. |
| `for_each` | array or object or string | A list or a map: one instance of the unit per item, read as each.key and each.value. |
| `approval` | one of `ask`, `auto` | ask to wait for approval of changes to this unit, auto to apply them without asking. |
| `timeout` | string | How long a program of the unit may run. |
| `protect` | boolean | Refuse to destroy the unit. |
| `retry` | [Retry](#retry) | Running the program again when it fails. |

## Unit › ready {#unit-ready}

When the service counts as started: a URL, a port, or a program; up waits for it.

| Key | Type | Description |
|---|---|---|
| `http` | string | A URL that answers with a 2xx status once the service is ready. |
| `tcp` | string | A host:port that accepts a connection once the service is ready. |
| `run` | string or list of string | A program that exits with 0 once the service is ready. |
| `interval` | string | How often the check runs. |
| `timeout` | string | How long up waits for the service to be ready. |

## Task {#task}

Exactly one of run, steps, or one call of a task.

| Key | Type | Description |
|---|---|---|
| `description` | string | What the task does, which help and list show. |
| `inputs` | map of [Input](#input) | Inputs of the task, each also a flag of its command. |
| `outputs` | map of [Output](#output) | Outputs of the task. |
| `run` | string or list of string | The program of the task. |
| `steps` | list of [Step](#step) | The steps of the task. |
| `shell` | one of `sh`, `bash`, `python`, `node`, `pwsh` | The shell that runs a script, also for each step; sh by default. |
| `dir` | string | The directory that the programs start in, relative to this file, also for each step. |
| `env` | map of string or [Env](#env) | Variables set for the programs of the task and its steps. |
| `pass` | list of string | Variables of the environment that the programs of the task and its steps get. |
| `stdin` | [Stdin](#stdin) | What the program reads on its standard input. |
| `requires` | string or list of string | Programs that must be in PATH before the task runs. |
| `needs` | list of string | Tasks that run before this one. |
| `if` | string or boolean | A condition that skips the task when false. |
| `for_each` | array or object or string | A list or a map: the task runs once per item, read as each.key and each.value. |
| `max_parallel` | integer | How many items of for\_each run at once; OPSTRUSS\_PARALLEL by default. |
| `on` | [Triggers](#triggers) | Triggers that run the task from the daemon of the stack. |
| `approval` | one of `ask`, `auto` | ask to wait for approval of the changes that the task makes, auto to apply them without asking. |
| `concurrency` | one of `queue`, `cancel`, `skip`, `parallel` or [Task › concurrency](#task-concurrency) | What a new run of the task does while another goes on, also across the tasks of one group. |
| `timeout` | string | How long the task may run. |
| `stop` | [Stop](#stop) | How its program is stopped. |
| `retry` | [Retry](#retry) | Running the task again when it fails. |
| `failed_when` | string or boolean | A condition on the outputs that marks the task failed. |
| `changed_when` | string or boolean | A condition on the outputs that says whether the task changed something. |
| `continue_on_error` | boolean | Record a failure and go on. |
| `internal` | boolean | Leave the task out of the commands and the MCP tools; other tasks and its own on: still run it. |

Any other key is a call of a task by that name, with its inputs as the value.

## Task › concurrency {#task-concurrency}

What a new run of the task does while another goes on, also across the tasks of one group.

| Key | Type | Description |
|---|---|---|
| `group` (required) | string | A name that the tasks whose runs wait for one another share. |
| `mode` | one of `queue`, `cancel`, `skip`, `parallel` | What a new run does: queue, cancel, skip, or parallel; queue by default. |

## Step {#step}

Exactly one of run, task, block, or one call of a task.

| Key | Type | Description |
|---|---|---|
| `name` | string | The name that the output shows for the step. |
| `id` | string | The id that later steps read as steps.&lt;id&gt;. |
| `if` | string or boolean | A condition that skips the step when false. |
| `run` | string or list of string | A program. |
| `task` | string | A task of the project, or a standard task such as up, to run. |
| `block` | list of [Step](#step) | Steps that rescue and always go with. |
| `rescue` | list of [Step](#step) | Steps that run when a step of block fails, which then counts as handled. |
| `always` | list of [Step](#step) | Steps that run after block and rescue, whether they failed or not. |
| `with` | object | Inputs of the task of task:. |
| `units` | list of string | The units that task: up, destroy, status, or logs acts on. |
| `yes` | boolean | Approve the changes of task: up or destroy, as --yes does. |
| `force` | boolean | As --force for task: up or destroy. |
| `env` | map of string or [Env](#env) | Variables set for the program. |
| `pass` | list of string | Variables of the environment that the program gets. |
| `shell` | one of `sh`, `bash`, `python`, `node`, `pwsh` | The shell that runs a script. |
| `dir` | string | The directory that the program starts in, relative to this file. |
| `stdin` | [Stdin](#stdin) | What the program reads on its standard input. |
| `timeout` | string | How long the step may run. |
| `for_each` | array or object or string | A list or a map: the step runs once per item, read as each.key and each.value. |
| `retry` | [Retry](#retry) | Running the step again when it fails. |
| `failed_when` | string or boolean | A condition on the outputs that marks the step failed. |
| `changed_when` | string or boolean | A condition on the outputs that says whether the step changed something. |
| `continue_on_error` | boolean | Record a failure and go on with the next step. |
| `outputs` | map of [Output](#output) | Outputs of the program of the step. |

Any other key is a call of a task by that name, with its inputs as the value.

## Triggers {#triggers}

What runs a task from the daemon of the stack.

| Key | Type | Description |
|---|---|---|
| `schedule` | string or [Schedule](#schedule) or list of string or [Schedule](#schedule) | Cron schedules, in the time zone of the machine unless timezone names another. |
| `every` | string or list of string | Intervals, such as 5m, from when the daemon starts. |
| `webhook` | [Webhook](#webhook) or list of [Webhook](#webhook) | HTTP requests to the daemon that run the task. |
| `watch` | [Watch](#watch) or list of [Watch](#watch) | Events from files, the output of a service, or a program that writes a line per event. |
| `after` | [After](#after) or list of [After](#after) | Runs of other tasks or commands that this task follows. |

## Schedule {#schedule}

A cron schedule, as a string or with its options.

| Key | Type | Description |
|---|---|---|
| `cron` (required) | string | A cron expression of five fields, or a shorthand such as @daily. |
| `timezone` | string | An IANA time zone, such as Asia/Jakarta; the time zone of the machine by default. |
| `persistent` | boolean | Run once what was missed while the daemon was not running or the machine slept. |
| `if` | string or boolean | A condition on the event that skips the run when false. |
| `with` | object | Inputs of the task for runs of this trigger. |

## Webhook {#webhook}

An HTTP request to the daemon that runs the task, checked by its signature.

| Key | Type | Description |
|---|---|---|
| `path` (required) | string | The path that the daemon serves the webhook at. |
| `method` | one of `GET`, `POST`, `PUT`, `PATCH`, `DELETE` | The HTTP method; POST by default. |
| `secret` (required) | string | The secret that signs the requests, as ${{ secrets.&lt;name&gt; }}. |
| `signature` (required) | one of `github`, `gitlab`, `slack`, `stripe`, `standard`, `bearer` or [Webhook › signature](#webhook-signature) | How a request is checked: the scheme of github, gitlab, slack, stripe, standard webhooks, or bearer, or a header of your own. |
| `respond` | one of `accepted`, `wait` | accepted to answer 202 with the id of the run at once, or wait to answer with the outputs of the task. |
| `if` | string or boolean | A condition on the event that skips the run when false. |
| `with` | object | Inputs of the task for runs of this trigger. |

## Webhook › signature {#webhook-signature}

How a request is checked: the scheme of github, gitlab, slack, stripe, standard webhooks, or bearer, or a header of your own.

| Key | Type | Description |
|---|---|---|
| `header` (required) | string | The header that holds the signature. |
| `algorithm` | one of `sha256`, `sha512` | The hash of the HMAC. |
| `encoding` | one of `hex`, `base64` | How the signature is written. |
| `prefix` | string | Text before the signature in the header, such as sha256=. |

## Watch {#watch}

Exactly one source: unit, paths, or one call of a task.

| Key | Type | Description |
|---|---|---|
| `unit` | string | A service whose output lines are the events. |
| `paths` | list of string or [Paths](#paths) | Files to watch, relative to this file. |
| `stream` | one of `stdout`, `stderr`, `both` | Which output of the service: stdout, stderr, or both; both by default. |
| `if` | string or boolean | A condition on the event that skips the run when false. |
| `batch` | string | Gather the events of this long into one run. |
| `debounce` | string | Run once no event has come for this long. |
| `with` | object | Inputs of the task for runs of this trigger. |

Any other key is a call of a task by that name, with its inputs as the value.

## After {#after}

Running after a run of another task or command ends.

| Key | Type | Description |
|---|---|---|
| `task` (required) | string | The task, or the command such as up, whose runs this task follows. |
| `unit` | string | Only runs that touched this unit. |
| `outcome` | one of `success`, `failure`, `any` | Which runs: success, failure, or any; success by default. |
| `if` | string or boolean | A condition on the event that skips the run when false. |
| `with` | object | Inputs of the task for runs of this trigger. |

## Moved {#moved}

Addresses that changed, from the old to the new.

| Key | Type | Description |
|---|---|---|
| `from` (required) | string | The old address. |
| `to` (required) | string | The new address. |

## Removed {#removed}

Units gone from the files.

| Key | Type | Description |
|---|---|---|
| `from` (required) | string | The address of the unit that left the files. |
| `destroy` (required) | boolean | true to destroy what the unit made, false to forget its record and leave it. |

:::
