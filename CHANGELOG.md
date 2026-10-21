# Changelog

Every change to OpsTruss that an operator, or the author of a component or
a plugin, can notice is written down here, newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
with one addition: a release that asks something of a project, a component,
or a plugin says so first, under "Upgrade notes". OpsTruss follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0.0 a
minor release can change what a project, a component, or a plugin relies on.

## [Unreleased]

### Added

- `opstruss validate` checks `opstruss.yaml`, its include files, and every
  stack against the schema and the rules of the model, among them that
  every `${{ }}` compiles and reads only what exists and may be read where
  it is written, and that units, task calls, and locals form no cycle. It
  names the file, line, and column of each problem; `--json` prints one
  document.
- `uses:` names local plugins (`./<path>` with `opstruss.plugin.yaml`),
  components, directories of components, and built-in tasks under another
  name. `opstruss validate` reads the `opstruss.plugin.yaml` of each and
  checks it: standard tasks and hooks only in a plugin with `up`, no
  `pass:` and no `env:` on inputs, and tasks that are one operation each,
  which read only their inputs and steps and call only built-in tasks. It
  also checks what each unit's `uses:` and each call of a task by name
  stand for, and the inputs given to them. Git sources do not run yet.
- A task of a plugin is called by name, as `<name>.<task>: {…}`: its
  inputs are those of the plugin and the task, filled by the call and the
  declaration's `with:`; a program in argv starts from the plugin's
  directory (`OPSTRUSS_PLUGIN_DIR`) and runs in the caller's; it gets only
  the variables its `env:` asks for that `pass:` of the step, task, or
  declaration lets through; and its expressions read only its inputs,
  steps, and the facts.
- `requires:` of a task, and of the plugin of a task, is checked before
  it runs: a program that is not in `PATH` fails the task with its name.
- A unit with `uses: <plugin>` runs the plugin's `plan`, `up`, and `down`
  with its `with:` as inputs. Its definition, and so its digest, holds the
  plugin's source by the content of its directory, so a changed script
  runs the unit again. The plugin's `plan` runs under the unit's lock,
  also in the plan before `up`, and writes `$OPSTRUSS_PLAN`: a unit whose
  definition did not change runs only when that plan has changes, `up`
  gets the plan's own digest as `OPSTRUSS_DIGEST`, and `units.<n>.plan`
  shows its changes and whether it is exact. The record keeps the inputs,
  the outputs the plugin declares, and the program of `down` with the
  plugin's directory, which `down` runs; a plugin without `plan` runs as a
  `run` unit does, and one without `down` is forgotten.
- A plugin with `state: tool` keeps the tool's own state in `tool/` of
  each unit, which its programs get as `OPSTRUSS_STATE_DIR` and which goes
  with the record: `moved:` moves it, `down` removes it, and `removed:`
  with `destroy: false` sets it aside. A unit whose `tool/` holds state
  while its input `backend` names another place stops at plan, and two
  units with the same `backend` are refused. A plugin with `outputs: tool`
  gives every output its tool writes to `$OPSTRUSS_OUTPUT`, with their
  types. A unit cannot be named `tool`.
- A unit that uses a task, of the project (`uses: build`), of a plugin
  (`uses: docker.build`), or built in (`uses: write`), runs it at `up`
  with its `with:` as inputs, as a `run` unit runs its program: the
  task's outputs become the unit's, its programs and steps are the unit's
  (with `OPSTRUSS_WORKDIR` and `$OPSTRUSS_STATE`, and `opstruss.state_key`
  and `opstruss.reason` in its expressions), and the unit's `down:` takes
  it away. Its definition holds the task as written, the tasks it calls,
  and the programs they name by a relative path. A task that runs `plan`,
  `up`, or `down` of units cannot be used by a unit.
- A unit that uses a component (`uses: ./components/dns`, or
  `<name>.<component>` from a directory of components) runs the units
  inside at addresses under its own, such as `prod/dns/zone` and
  `prod/site[eu]/db`, and each instance keeps a record of its own with its
  source and inputs, which goes once nothing inside has a record. The
  component's files are read and checked on their own: the names in its
  `uses:` are its own, `stacks`, `secrets`, `backend`, `runtime`, and
  `mcp` are left out, and `secrets.*` is refused, so values come in
  through its inputs: the unit's `with:`, then their `env:` and
  `default:`. Units outside read only the component's `outputs`, and
  `units.<name>` also holds the outcome, changed, and plan of everything
  inside; the graph goes through the component, so a unit depends only on
  the units inside whose values it reads. `protect`, `approval`, `env`,
  `pass`, and `needs` of the unit reach every unit inside, with `protect`
  and `approval: ask` from any level, while `timeout`, `retry`, and `stop`
  are their defaults and `if` and `for_each` make instances; keys that
  only a program has are refused. A name on the command line or in
  `units:` picks every unit inside a component, one instance
  (`site[eu]`), or a unit inside (`site/db`); `moved:` moves an instance
  with everything inside, and `removed:` decides for everything inside a
  component unit that left the files. The tasks of a component, and
  `moved:` and `removed:` in one, do not run yet.
- `opstruss status [unit]...` (also `ps`) runs the `status` task of each
  unit's plugin without the unit's lock, with its record to read, and for
  other units says what the record holds: up with its last run and
  outputs, pending, or absent.
- `plan` exits 1 when a unit cannot be planned; a unit that another run
  holds still leaves it at 0.
- `plan` fails a unit whose `requires:` names a program that is not in
  `PATH`, unless a unit it depends on runs first in the same run; then it
  warns and checks again at the unit's turn.
- Built-in tasks, called by name in a step (`http: {…}`) or as the one
  call of a task, which passes its outputs on: `command` (`argv` or
  `script`, `shell`, `stdin`), `http` (JSON bodies parsed, lower-case
  header names, `status_code` with patterns such as `2xx`, extra roots
  from `ca`, and `tls` with the certificates of an https answer), `read`,
  `write` (only when the content or mode differs, through a rename),
  `set`, `assert`, `wait` (seconds, a TCP port, or a URL), and `log`. Each
  is also `opstruss.<name>`, and `retry` evaluates the inputs of a call
  again at each attempt.
- `kv` keeps one JSON value per name for the stack between runs, in the
  backend under `<stack>/_kv/`: a `kv` step without `value` reads, with
  `value` writes, and `value: null` removes; `ttl` makes a value expire,
  `expect` writes only over the value it names and otherwise fails with
  the value there is, and `changed` says whether the value changed.
  `kv('<name>')`, `kv('<name>', <default>)`, and `kv('<prefix>*')` read
  it in expressions, also of units and locals: one read per plan or up of
  units, and per evaluation of a task or step, read again at each attempt
  of `retry`.
- `opstruss explain` prints the project of one stack as read: include files
  and stack overrides merged, each project input with where its value came
  from (`--input`, the stack, `env`, or `default`), the value of each local,
  and the order of the units; names pick units and tasks with what they
  depend on.
- `${{ }}` is CEL with the cel-go extensions for strings, lists, math, sets,
  two-variable comprehensions, bindings, encoders, and optional values, plus
  `toJSON()`, `fromJSON()`, `toYAML()`, `fromYAML()`, `sha256()`, `file()`,
  and `strftime()` on timestamps. A value that is one `${{ }}` keeps its
  type, and maps are iterated in key order.
- `opstruss state list`, `pull`, `push`, and `rm` read and repair the
  records of a stack under their locks: `push` takes the etag of a `pull`,
  or `--force`, and refuses a stale copy, a record for another address,
  and a record of a newer OpsTruss; `rm` forgets a record without taking
  anything down and leaves a tombstone, and removes a tombstone.
- State lives in `.opstruss/state/`, one record per address, written
  atomically with a backup of the previous content. `_meta` names the
  project a stack belongs to, and a stack of another project, of a newer
  layout, or with records but no `_meta` is never written.
- `opstruss plan`, `up`, and `down` (also `preview`, `apply`, and
  `destroy`) run the `run` units of a stack in graph order, units that do
  not depend on each other in parallel, at most `OPSTRUSS_PARALLEL` (10) at
  once. Names and patterns with `*` or `name[key]` pick units; `plan` and
  `up` take what they depend on, `down` what depends on them, unless
  `--no-deps`.
- A unit runs again when its definition, a file of `sources`, `revision`,
  or a `pass` value changes, or a file of `generates` is missing;
  `up --force` runs the units named anyway. A unit with `down:` whose
  `pass` values differ from its record is refused, since its down reaches
  the old target, unless `up --force` moves it and forgets that target.
  `if` and `for_each` decide
  which instances exist, from what is known when the run starts.
- A change needs approval: `approval: auto`, or `--yes` for the run;
  otherwise the unit stops at plan, the units that depend on it are
  skipped, and the run exits with 2.
- Each unit holds the lock of its address from its plan to the end of its
  program, waiting while another run holds it; a run nested in a program
  of another fails at once on a lock that run holds.
- `up` marks the record pending before the program runs and keeps what a
  failed attempt parsed, so the next attempt gets it in `$OPSTRUSS_STATE`
  with `OPSTRUSS_REASON=interrupted`; programs that a killed OpsTruss left
  running are waited for first. `retry`, `timeout`, and `requires:` apply
  to units. A record the backend refuses at the end is kept in
  `.opstruss/errored/`, while what the program made stays in pending for
  the next `up`, which then removes the kept record.
- `moved:` carries a record, and the tool's state of a local unit, to its
  new address on the next `up`, in stages that a later run finishes if
  one is interrupted, and leaves a tombstone that older files stop at.
  Entries chain; a target that has a record of its own is refused, and so
  is a source whose last attempt did not finish.
- An attempt that did not finish with another down contract, such as other
  `pass` values, is kept as abandoned when `up` runs again, shown in the
  plan, and torn down after that `up` succeeds, with the values it
  recorded; one with the contract of the record is dropped without down.
- `down` runs the `down:` recorded for each unit, units that depend on
  others first, with the context it was written with: it refuses `pass`
  values that differ unless `--force`, and nothing goes down while a unit
  it reaches has `protect: true`. Instances that leave `if` or `for_each`
  go down at `up`; units that leave the files stay as orphans until
  `removed:` takes them down or forgets them, which leaves a tombstone.
- Every task of the project that is not `internal`, and every task of its
  units, is a command: `opstruss <task>`, also `opstruss run <task>`. Its
  inputs are flags with types (`--name value`, `--name` and `--no-name` for
  a boolean, a repeated flag for an array, JSON or `@file` for an object),
  and what comes after `--` fills the input with `args: true`. The program
  of a task gets its inputs as `INPUT_<NAME>` and in `$OPSTRUSS_INPUTS`, and
  its outputs are printed at the end.
- `steps:` run in order: `run`, `task:` (`plan`, `up`, or `down` of
  `units:`, a project task with `with:`, or the task of each unit that has
  it), and `block:` with `rescue:` and `always:`. `if`, `for_each`, `retry`,
  `timeout`, `failed_when`, `changed_when`, and `continue_on_error` apply to
  steps; `steps.<id>` holds `outputs` (with `stdout`, `stderr`, and
  `exit_code` for `run`), `changed`, and `outcome`, and `value:` gives the
  outputs of a task with steps, also when it failed, as far as they can be
  computed.
- `needs` runs each task it names once per run; `for_each` on a task runs
  its instances in parallel, at most `max_parallel` at once. A task with
  `approval: ask` stops without `--yes`, and `yes: true` on a `task:` step
  approves that call, but only plans this run read as exact, with the same
  digest.
- `concurrency` keeps one call of a task at a time in a stack and checkout,
  through a lock file in `.opstruss/tasks/<stack>/`: `queue` waits, `skip`
  leaves the call out, `cancel` stops the run that holds it when that run
  started with the same task, `parallel` takes no lock, and `group` gives
  one lock per value.
- The exit code of a task follows the outcome of the task called: 0, 1
  when it failed, and 2 when it stopped at plan for approval.
- At a terminal, `up`, `down`, and a task with `approval: ask` ask before
  each change, one question at a time, and only `y` or `yes` applies it;
  the time an answer takes does not count toward `timeout`. A required
  input of a task that has no value is asked for there too.
- `opstruss list` shows the tasks with their last run and the units with
  their state; `opstruss outputs` prints the outputs of the project from
  the stored outputs of its units, read without a lock; `opstruss runs`
  lists the runs of the stack, newest first, and `opstruss runs <run>`
  shows the events of one again. With `--json`, each prints one document,
  and `runs <run>` the events as they are stored.
- `OPSTRUSS_LOG=debug` writes the diagnostics of the engine to stderr.
- Every run keeps `record.json` (who ran what and when, the units it
  touched with their outcome, plan, and approval, and the outcome, exit
  code, and outputs of the run) and `events.jsonl` in
  `.opstruss/runs/<stack>/<run>/`, whose name an exclusive mkdir makes
  unique. A run stores at most 10 MiB of program output; the middle of
  more is dropped, with one diagnostic event.
- `--json` prints every event of a run on a line of its own, with `v`,
  `seq`, `time`, `run`, `type`, `level`, and `message`, and the address,
  task, step, or stream it belongs to. The last event, of type `run`,
  carries the outcome, the exit code, and the outputs of a task.
- A run that a crash left unfinished is marked cancelled, as interrupted,
  by the next run of its stack, and its temporary files go once none of
  its programs is left.
- Old runs go: a stack keeps 100 runs per task and at most 1 GiB of runs,
  and never drops a run still going or the last run that touched a unit.
- Programs of units and tasks start from a clean environment (`PATH`,
  `HOME`, `USER`, the locale, `TZ`, `TMPDIR`, `TERM`, proxies,
  `SSL_CERT_*`, `SSH_AUTH_SOCK`) plus the variables that `pass:` names and
  the values of `env:`, where `{file: …}` gives the path of a 0600 file.
  On top come the facts of the program contract, such as
  `OPSTRUSS_CONTRACT`, `OPSTRUSS_RUN`, `OPSTRUSS_TASK`, and
  `OPSTRUSS_ADDRESS`, and the inputs of a task as `INPUT_<NAME>` and in
  `$OPSTRUSS_INPUTS`.
- A program writes its outputs to `$OPSTRUSS_OUTPUT` as `name=value`
  lines, `name<<EOF` blocks, or one JSON object, and `changed`, `message`,
  and `sensitive` to `$OPSTRUSS_RESULT`. An output can also come from
  stdout, stderr, or a file, read as `text`, `json`, `yaml`, `lines`, or
  `dotenv`, the formats of `stdin:` too. The outputs of a unit or task take
  at most 1 MiB.
- Scripts run through `sh -e`, `bash --noprofile --norc -eo pipefail`,
  `python3 -c`, `node -e`, or `pwsh -NoProfile -NonInteractive -Command`,
  as `shell:` says.
- Each program runs in its own process group. A cancel or a timeout sends
  `stop.signal` (default `SIGINT`) to the group, waits `stop.grace`
  (default one minute), and kills what is left; processes that a program
  leaves in its group when it exits are stopped the same way.
- Every program holds the lock of its run on file descriptor 3, which
  tells OpsTruss that programs of an interrupted run still live, also
  where their environment cannot be read; a program leaves it open.
- `opstruss validate` rejects `env:` keys and `pass:` names that start with
  `OPSTRUSS_` or `INPUT_`, task inputs that become the same `INPUT_`
  variable, and an unknown `stop.signal`.
- `-C <dir>`, `--stack` (or `OPSTRUSS_STACK`), `--input name=value`, and
  `--json`.
- `opstruss --completion bash|zsh|fish` prints a completion script.
- `opstruss version` prints the version and the platform.
- Release builds for Linux and macOS on amd64 and arm64, with checksums,
  SBOMs, and build provenance attestations.

[Unreleased]: https://github.com/opstruss/opstruss/commits/main
