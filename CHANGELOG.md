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

- Official remote plugins and components from `github.com/opstruss/plugins`
  resolve to verified Git snapshots in a shared per-user cache. Transitive
  pins live in `opstruss.lock`; `--locked` refuses pin changes. The `plugins`
  commands list, fetch, explicitly update, and prune dependencies, including
  all stacks for fetch and update. Explicit fetch repairs corrupt cache
  entries from the existing pin. Help and completion do not fetch sources.
  Unit digests include the remote plugins called by project tasks.
- Plugin `bin:` declarations download official release executables or
  `tar.gz` payloads with SHA-256 verification and a private plugin PATH.
  Cleanup restores the source and binary pins of each stored contract,
  including pending and abandoned attempts after updates or cache removal.
  Records now use version 2 and the daemon API uses protocol 2; older
  local records remain readable.

- `opstruss up -d` runs up in the daemon of the stack, which it starts in
  the background when none runs; a task takes `-d` as well. The daemon
  stays until `opstruss stop`, which sends it SIGTERM once its pid and
  start time match `.opstruss/daemon/<stack>.json`, then waits for it to
  end. While a daemon runs, `plan`, `up`, `down`, `status`, and tasks run
  in it with the caller's files and environment and ask at the caller's
  terminal, so they share its locks and concurrency; Ctrl-C cancels such a
  run, a second Ctrl-C kills its programs at once, and one whose caller
  goes away runs on without answers. The daemon
  listens on a socket for its own user only, serves only its project and
  stack, reads messages of at most 16 MiB, refuses a caller of another
  protocol with what to do, writes its events to
  `.opstruss/logs/<stack>/_daemon.jsonl`, which turns over at 10 MiB into
  `.1` and `.2`, and stops when its lock file is deleted. An event that
  this log, or the log of a service, cannot take is lost, and the next
  one written says first how many were lost and why.
- A unit with `service:` is a program that keeps running, which the daemon
  of the stack keeps. `up` starts it, and again after its definition
  changed, with `--force`, or when it does not run, and waits until
  `ready:` passes: one `http`, `tcp`, or `run` check every `interval`
  (default 1s) for at most `timeout` (default 1m). `restart:`
  (`on-failure` by default, `always`, or `no`) starts it again after it
  exits, 100 ms later at first and twice as long each time up to a
  minute, from the start again once it ran for 10 seconds. It stops with
  `stop.run`, then `stop.signal` (default `SIGTERM`) to its process group,
  and a kill after `stop.grace` (default 10s). `down` stops it and drops
  its record, while `opstruss stop` stops it with the daemon and keeps the
  record. Its output goes to `.opstruss/logs/<stack>/<unit>.jsonl`, which
  turns over as the daemon's log does, with the sensitive values of the
  run that started it hidden, and `status` gives its `state` (`running`,
  `starting`, `restarting`, or `stopped`), `pid`, `since`, and
  `restarts`.
- A run without a daemon that starts a service makes its process the
  daemon of the stack, which stays in the foreground with the output of
  its services until they end or Ctrl-C stops them, as `docker compose up`
  does; the runs of other command lines go to it meanwhile.
- A service runs in a session of its own with `OPSTRUSS_SERVICE_ID`, which
  its processes pass on, and a lock of its own on file descriptor 3, and
  is written in `.opstruss/daemon/<stack>.json` before it runs. When a
  daemon starts, and when `opstruss stop` finds none, what a daemon that
  ended left is stopped first. A process group whose leader is gone and
  that does not show the id keeps the service from starting and
  `opstruss stop` from finishing, naming its pid, until
  `opstruss unlock <unit>` forgets it.
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
  component unit that left the files.
- The tasks of a component are tasks of each of its instances:
  `opstruss check site` runs `check` in every instance of `site`, with
  the component's inputs, locals, and units of that instance, `needs`
  once per instance, and a `concurrency` lock of each instance. Their
  `task:` steps name the units and tasks of that instance, also in
  `steps.<id>.units`, and a unit inside may use one; `internal` ones are
  only called inside. `moved:` and `removed:` in a component name units
  inside it and apply in every instance. The events of the task of a
  unit or of an instance name it `<address>/<task>`, and the inputs of a
  task take names other than those of its project or component.
- `opstruss exec <unit> -- <program> [arg]...` runs a program of the
  operator with what the programs of one unit get (its env and pass, the
  inputs of its plugin or task as `INPUT_*`, its working directory, and
  `OPSTRUSS_WORKDIR`, `OPSTRUSS_STATE`, and `OPSTRUSS_STATE_DIR`), under
  the unit's lock and on the terminal, such as
  `opstruss exec dns/zone -- tofu state list`. A plugin with an `exec`
  hook prepares the tool first and gets the program after its own
  arguments, for `exec "$@"`. No plan, approval, or `protect` applies;
  the program's output stays its own, the events of the run go to
  stderr, and the program's exit code is the run's, which the record
  names `<unit>/exec` with its argv.
- A task with `on: schedule` or `on: every` runs by itself in the daemon
  of its stack. `cron` reads as CronJob of Kubernetes reads it, with names
  of months and days and `@daily` and the like; `timezone` (default: the
  machine's) holds the time zone, and around a change of daylight saving
  time a job at a fixed time that the clock skips runs right after the
  jump and one that the clock repeats runs once, as cronie runs them.
  `every` (at least 1s) runs one interval after the daemon installs it,
  and keeps its ticks through an `up` that leaves its interval.
  A time that comes while the run that the trigger made last still runs
  or waits goes to that run; one that comes more than a minute late, after
  the machine slept, is missed, and `persistent: true` runs once for what
  a schedule missed then or while no daemon ran. `if` and `with` of a
  schedule read `event` and decide whether a run is made and with what
  inputs. The daemon installs the triggers when it starts, with its own
  environment, and reconciles them after each `up` through it, as
  `docker compose up` does its containers: a trigger whose source stays
  (its interval, cron and time zone, paths, or the program of its watch
  with the environment that program gets) keeps it, and its next events
  take the files, inputs, and environment of that `up`, while one that
  changed starts again on its own; a moved trigger is still the same. An
  event is handled with one version of the project from its `if` to its
  run, a request to a webhook too while an `up` changes its secret, and
  what a watch gathered with the version before runs with that version. A
  project that does not load, or a watch that cannot be prepared, keeps the
  triggers installed before. `opstruss list` shows the triggers of each
  task and, while the daemon runs, when it comes next.
- A task with `on: watch` runs for each event of its source: a line that a
  service writes, or the `watch` hook of the plugin of a unit; a change of
  the files that `paths` names, relative to the directory of the task or
  absolute, with `${{ }}` read as the daemon installs the trigger, through
  inotify on Linux and the stat of each file every second on macOS, for at
  most 50,000 files; or a line of a program that one call of a task runs,
  `command` or a task of a plugin, which the daemon keeps running as it
  keeps a service and starts again with backoff, with its inputs read
  again each time. The program of a source works in the directory of the
  task or unit that watches, as a step does. `up` through the daemon, also
  `up -d`, and the start of the daemon return once each watch that they
  start is ready, with its files watched and its program started, so a
  change made after that makes an event; a watch that an `up` keeps keeps
  its baseline and program. `if` chooses each event, and `batch` or
  `debounce` gathers them into one run with `event.lines` or
  `event.paths`: the first that fit in 10,000 items or 1 MiB, with
  `event.truncated` counting the events left out, and `event.count` both;
  a file that changes again stays with its first change while it is in
  the list. A task
  with `on: after` runs when a run of another task, or of a command such
  as `up` or `plan`, ends in the daemon with the `outcome` it names, for
  the unit it names, with `event.task`, `unit`, `outcome`, and `outputs`;
  a chain of after stops past three.
- A task with `on: webhook` runs when a request comes to its `path` and
  `method` on `runtime.listen` of the daemon, which every project with a
  webhook writes. `signature` checks `secret` before any run is made:
  `github`, `gitlab`, `slack`, `stripe`, `standard`, `bearer`, or an HMAC
  over the body in a header of choice; the schemes with a timestamp refuse
  one more than five minutes away, and a refused request gets 401 and a
  line in the log of the daemon. `respond: accepted` answers 202 with the
  id of the run once its record is there, and `respond: wait` answers with
  the outcome and outputs of the run, sensitive ones hidden. `if` that
  leaves a request out answers 202 without a run, `with` that the task's
  inputs do not take answers 422, a task with 100 runs unfinished answers
  503 with `Retry-After`, a body above 1 MiB gets 413, and one that does
  not come within 10 seconds 408; a kept-alive connection closes after 60
  idle seconds. `event` holds
  `method`, `path`, `headers` by lower-case name, `query`, and `body`,
  parsed when it is JSON.
- `event.trigger` and `event.time` say what made a run and its logical
  time: the time of the schedule, or when a run from the command line
  started; the record of a run keeps `trigger`.
- `opstruss <task> --event <event>` runs a task of the project as one of
  its triggers runs it for an event that you write, without the daemon or
  a real request: `@<file>`, `@-` for stdin, or the JSON itself, one
  object of at most 4 MiB with `trigger` and the fields of one event as
  its source gives it, such as a webhook request or one changed file.
  The event picks the trigger of its kind by what tells them apart, such
  as `method` and `path`, and goes through the same `if`, filter of a
  watch, `batch` or `debounce` of that one event without waiting, and
  `with`; the flags of inputs go over `with`, also `false` and `""`, and
  an event that `if` leaves out exits 0 without a run. The run is one of
  the command line: its record keeps `trigger: cli` and `simulated` with
  the kind of trigger, and `-d` runs it in the daemon.
- `opstruss logs [unit|task]...` shows the output of the units: the log of
  a service, from the files it turned over into first; the `logs` task of
  a unit's plugin, with the input `follow` when the task declares it; and
  for other units the output of the last run that touched them, as
  `docker logs` shows that of a container that ended. `-f` (or `--follow`)
  goes on with the logs of services, also after they turn over, and with
  plugins that follow, until Ctrl-C. `logs` runs in the caller's process
  also while a daemon runs, and the lines it shows keep their own time and
  run; a `task: logs` step shows them too. A task, named as it is, shows
  its last run as `list` finds it: all of the run when the task started
  it, otherwise only the task's own events, after a line that names the
  run with `via`; `-f` follows that run until it ends, then each later run
  of the task, of the daemon or of another terminal, until Ctrl-C, after
  which `logs` exits with 0. A name of a unit and a task shows both, and
  one of neither fails.
- `opstruss status [unit]...` (also `ps`) runs the `status` task of each
  unit's plugin without the unit's lock, with its record to read, and for
  other units says what the record holds: up with its last run and
  outputs, pending, or absent. A `task: status` step gives what each unit
  says as `steps.<id>.units['<unit>'].outputs`, and `--json` carries it in
  the event of each unit.
- A sensitive value is hidden as `***` wherever a run writes it: the
  terminal, `--json`, events, `record.json`, error messages, and
  `explain`, line by line, in its JSON form, and quoted as error messages
  quote values, also inside values computed from it, such as
  `base64.encode()`, and when a run only reads it back from a record, as
  `down` does. A value of one `${{ }}` is sensitive as a whole, while a
  map or a list written in YAML keeps it to its keys or items, so the
  other fields beside a password stay readable, also as `each.value` of a
  `for_each` over it. Inputs with `sensitive: true` of the project, a
  task, or a plugin task are sensitive, and so are outputs that are
  declared so, that a program names in `sensitive` of `$OPSTRUSS_RESULT`,
  or that hold a sensitive value, with a warning that names them. A
  sensitive input is never taken from a flag or `--input`, but from its
  `env:` or typed without echo at the terminal. A sensitive value cannot
  be an item of argv, which `validate` checks where the files show it and
  the run checks before a program starts, and it is never a key of
  `for_each`, a `concurrency.group`, or a name in `units:`; `write` keeps
  sensitive content in a file with mode 0600. A value shorter than 4
  characters is not hidden, and the run warns about it. The value of a
  variable that `pass:` lets through to a program, a step, a hook of a
  unit, or a service, is hidden too when its name matches `*_TOKEN`,
  `*_SECRET`, `*_PASSWORD`, `*_ACCESS_KEY`, or `*_SECRET_KEY`, as
  Buildkite hides its redacted vars; one that `env:` sets in its place is
  not.
- What a run writes out (the terminal, `--json`, `events.jsonl`, `runs`,
  the log of a service, error messages) also hides the shapes of
  well-known credentials that the run was never given, as GitHub secret
  scanning and gitleaks find them: GitHub (`ghp_`, `gho_`, `ghu_`,
  `ghs_`, `ghr_`, `github_pat_`), GitLab (`glpat-`), Slack (`xoxa-`,
  `xoxb-`, `xoxp-`, `xoxr-`), Stripe live keys (`sk_live_`, `rk_live_`),
  and Anthropic (`sk-ant-`) tokens of their length, and a private key in
  PEM from its `BEGIN` line to its `END` line, or to the end of the
  output, also in one line of JSON. Such a shape does not make a value
  sensitive: an output that holds one stays as it is in the next step,
  the record, and the state. The list may grow, and it does not catch
  every credential, so a value to keep comes from a secret or is
  `sensitive`. A line of a program or a service longer than 64 KiB is cut
  as it comes, where no hidden value is cut in two.
- The data key of a stack, made the first time a value needs it, plan
  included, is kept in `<stack>/_meta`, wrapped with age inside OpsTruss
  for the age key of this machine, or for `encryption_keys`, and a run
  opens it once. Sensitive values
  of units, components, plugins, and `kv` are kept encrypted with it
  (AES-256-GCM, `enc:v1:`) and bound to their record or name: inputs,
  outputs, the env of `down`, and, for a unit with sensitive values, what
  its program wrote to `$OPSTRUSS_OUTPUT` when it could not be read, also
  in `pending`, abandoned attempts, and `.opstruss/errored/`. The digest
  of a unit hashes its sensitive values with HMAC, under a key from the
  data key and the id of the record, so a new value runs the unit again
  and the digest tells nothing of it. A unit with a sensitive value fails
  before it runs when the stack has no way to its data key, and an output
  that a program marks sensitive there is not kept. `opstruss state pull`
  shows the values encrypted.
- The age key of this machine is the first X25519 identity in
  `$OPSTRUSS_AGE_KEY`, in `$OPSTRUSS_AGE_KEY_FILE` or else
  `opstruss/age/keys.txt` in the user's configuration directory, then, as
  sops finds its keys, in `$SOPS_AGE_KEY`, in `$SOPS_AGE_KEY_FILE` or else
  `sops/age/keys.txt`. The first stack that needs one makes it in the file
  of OpsTruss, as age-keygen writes it, and the run says where. `pass:`
  never passes `OPSTRUSS_*`, so no program gets it.
- `encryption_keys:` lists what opens the data key of each stack, for when
  a second machine, such as a teammate's or CI, opens the same state; a
  stack replaces the list of the project as a whole, and a component
  ignores it. A string is an age X25519 recipient, which OpsTruss opens
  itself. A map is one key that the `sops` program wraps for, named as in
  the creation rules of `.sops.yaml` (`kms`, `gcp_kms`, `azure_keyvault`,
  `hc_vault_transit_uri`, or `pgp`) and passed to its flag as written,
  with `--config /dev/null`, so `.sops.yaml` plays no part. `_meta` keeps
  an `age` and a `sops` envelope, each with the keys it was wrapped for: a
  run tries every age key of this machine, and runs sops only when age
  cannot open it. When the list and an envelope differ, `plan`, `up`,
  `down`, and runs of tasks wrap that envelope again at their start, under
  the lock of `_meta`, and leave the data key as it is; a machine that
  cannot open it yet only warns, and a list that would leave the data key
  closed to the machine that writes it stops the run before any unit.
  Without the list, `_meta` stays as it is. A data key that does not open
  names the keys that wrap it and the age key of this machine to add.
  `validate` checks the list without any key: not empty, no key twice, no
  `${{ }}`, an age secret key refused without being printed, and ssh and
  plugin recipients not supported yet.
- `opstruss outputs` prints sensitive outputs as `***`, and as they are
  with `--show-sensitive`; `status` and `list` hide them too.
- `opstruss doctor` checks that this machine has what a run of the stack
  needs, without running anything or resolving a secret: each program
  that `requires:` names for an instance of a unit or a task, with its
  path; when the stack needs a data key, the age key of this machine and
  where it comes from, the key that opens the data key, or whether a run
  here can make it, and how `encryption_keys` differs from what wraps it;
  sops 3.10.0 or newer when a secret comes from a sops file or
  `encryption_keys` has a key for sops; the variables that a plugin asks
  for and no `pass:` lets through; and
  whether `.opstruss/` is on a network filesystem. Each finding is `ok`,
  `skip`, `warn`, or `error`, and only an `error`, which fails a run,
  makes it exit 1; `--json` prints one document.
- `secrets:` names where each secret of the project comes from: a sops
  file, whole or one `field` of it; a variable of the environment (`env:
  <NAME>`); what a program prints (`run:`); or the output `value` of a
  task of a plugin, called by name. A run resolves a secret only when an
  expression of a unit or task it runs reads it, `plan` included, once a
  run, and decrypts each sops file once; a source that fails fails what
  reads it, with its name, and the rest runs. Values computed from a
  secret, in locals and in the inputs and outputs of components, wait as
  long. The expressions of `on:` read secrets that sops and env hold, also
  outside a run, as the daemon installs a webhook or starts a watch
  source, and `validate` refuses there one that a program or a task gives,
  which only a run reads. A secret is sensitive, a stack overrides it by
  name, and commands that only read never resolve one. `validate` checks
  that each has one source, whose expressions read the stack, the
  project's inputs, locals that read no secret, and the facts known at
  plan, and that `if`, `for_each`, and the outputs of the project read no
  local that reads a secret; a key of a secret that is no source and not
  the map of a call, such as `sopz:`, is named as a typo of the source
  nearest to it.
- A task input with `env:` takes its value from that variable when the
  call gives none, before the default.
- `opstruss init` sets up the project in its directory, adding only what
  is missing: `opstruss.yaml` with `opstruss:` and a `name` made from the
  directory's name (or `--name`), and `.opstruss/` in `.gitignore`. With
  `--stack`, it also makes that stack's `_meta`; `--json` prints what it
  wrote. It makes no key: the first command that needs one does.
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
  `set`, `assert` (conditions in `that`, with or without `${{ }}`, as in
  `if`), `wait` (seconds, a TCP port, or a URL), and `log`. Each is also
  `opstruss.<name>`, and `retry` evaluates the inputs of a call again at
  each attempt.
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
  type, and maps are iterated in key order. `validate` refuses a field of
  `steps.<id>` other than `outcome`, `changed`, `outputs`, and `units`, and
  an output that a step is known not to give, with the name it likely
  meant; errors say when a function is read as a key, that `$NAME` reads
  no environment variable, and at run time which keys a map has when one
  is missing.
- `opstruss state list`, `pull`, `push`, and `rm` read and repair the
  records of a stack under their locks: `push` takes the etag of a `pull`,
  or `--force`, and refuses a stale copy, a record for another address,
  and a record of a newer OpsTruss; `rm` forgets a record without taking
  anything down and leaves a tombstone, and removes a tombstone. `list`
  shows the keys of `kv` too, and `rm _kv/<name>` removes one under the
  lock of `kv`, with the etag of a `pull` or `--force`, such as a claim
  that a run left.
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
  inputs are flags with types (`--name value`, `--name`, `--name=false`,
  or `--no-name` for a boolean but not two of them, a repeated flag for an
  array, JSON or `@file` for an object), and what comes after `--`, or its
  own flag but not both, fills the input with `args: true`. A task that
  several units have is one command with the inputs of all of them: each
  unit reads a flag by the type of its own input, and a call that one of
  the units it reaches does not take runs in none of them. `validate`
  refuses an input whose flag is one of OpsTruss, such as `--yes` or
  `--event`, or that of another input. The program
  of a task gets its inputs as `INPUT_<NAME>` and in `$OPSTRUSS_INPUTS`, and
  its outputs are printed at the end. In `--help`, a task without a
  `description` shows the first line of its `run`, the ids of its steps,
  or the task it calls.
- `steps:` run in order: `run`, `task:` (`plan`, `up`, or `down` of
  `units:`, a project task with `with:`, or the task of each unit that has
  it), and `block:` with `rescue:` and `always:`, which read the outcome
  of `block:` as `steps.<id>.outcome`. `if`, `for_each`, `retry`,
  `timeout`, `failed_when`, `changed_when`, and `continue_on_error` apply to
  steps; `steps.<id>` holds `outputs` (with `stdout`, `stderr`, and
  `exit_code` for `run`), `changed`, and `outcome`, and for a `task:` step
  of units `units`, one entry for each instance it touched, also one that
  `up` took down or forgot, which `task: plan` lists already with a plan
  of its own (`delete` or `forget`, exact, with a digest of the operation
  and the record); `value:` gives the outputs of a task with
  steps, also when it failed, as far as they can be computed.
- `needs` runs each task it names once per run; `for_each` on a task runs
  its instances in parallel, at most `max_parallel` at once, starting them
  in the order of their keys. A task with `approval: ask` stops without
  `--yes`, and `yes: true` on a `task:` step approves that call, but only
  plans this run read as exact, with the same digest, also the down of an
  instance that left `if` or `for_each`.
- `concurrency` keeps one call of a task at a time in a stack and
  checkout, through a lock file in `.opstruss/tasks/<stack>/`: `queue`
  waits, `skip` leaves the call out, `cancel` stops the run that holds it
  when that run started with the same task, in another process through a
  request in its `cancel.jsonl` with reason `concurrency`, `parallel`
  takes no lock, and
  `group` gives one lock per value. It holds the tasks of the project as
  the command line, MCP, a trigger, or a step `task:` calls them; a task
  of a plugin, which is one operation, and the task that a unit runs,
  which the lock of the unit's address holds, take no lock.
- The exit code of a task follows the outcome of the task called: 0, 1
  when it failed, and 2 when it stopped at plan for approval.
- At a terminal, `up`, `down`, and a task with `approval: ask` ask before
  each change, one question at a time, and only `y` or `yes` applies it;
  the time an answer takes does not count toward `timeout`. A required
  input of a task that has no value is asked for there too.
- `opstruss list` shows the tasks with their last run, also one that ran
  them inside the run of another task, which it names after `via`, and the
  units with their state; `opstruss outputs` prints the outputs of the
  project from the stored outputs of its units, read without a lock;
  `opstruss runs` lists the runs of the stack, newest first, and
  `opstruss runs <run>` shows the events of one again, and with `-f` goes
  on with a run that still runs until it ends; neither needs the inputs
  of the project. With `--json`, each prints
  one document, and `runs <run>` the events as they are stored.
  `opstruss runs cancel <run>` cancels one run wherever it runs, at
  another terminal, in the daemon, or waiting for a lock, and nothing else
  there: it appends a request to `cancel.jsonl` in the run's directory,
  which the run reads within 200 ms, so no signal goes to a pid. The run
  stops as at a first Ctrl-C, or with `--force` as at a second one, also
  as the first request; it ends `cancelled` with reason `requested`, its
  events name who asked, and its process exits 1. `runs cancel` waits
  until the run ends and exits 0 when it ended cancelled, 1 otherwise.
  `opstruss runs rerun <run>` runs a task, `up`, or `down` again as a new
  run with `rerun_of` in its record: the units it chose, `--input`, the
  flags and `with` of its caller, and its event, through the project as
  it is now. `if:` of its trigger is not asked again, `with:` of that
  trigger is, approval, `--yes`, and `--force` are the new caller's, and
  the environment is read again. What came from outside is sealed whole
  for the age key of the machine in a `replay` file beside the record;
  a run on a machine without one goes on and says it cannot run again, and
  a run whose payload is missing, damaged, or sealed elsewhere, or that an
  older build recorded, does not run again. Records
  and events of a newer OpsTruss stop them with what to do, and a record
  that cannot be read names the run to repair or remove.
- `OPSTRUSS_LOG=debug` writes the diagnostics of the engine to stderr.
- Every run keeps `record.json` (who ran what and when, the units it
  touched with their outcome, plan, and approval, also the instances that
  `up` took down or forgot, the tasks of the project that ran inside it
  through `task:`, `needs`, or a unit, with one outcome each, and the
  outcome, exit code, and outputs of the run) and `events.jsonl` in
  `.opstruss/runs/<stack>/<run>/`, whose name an exclusive mkdir makes
  unique; `plan`, `status`, and `logs` only read, so theirs goes once they
  finish, and a finished run keeps `record.json`, `events.jsonl`, and
  `artifacts/` when it holds something. Events reach `events.jsonl` within
  200 ms. A run stores at most 10 MiB of program output; the middle of
  more is dropped, with one diagnostic event. Events that cannot be
  written, as on a full disk, do not stop the run or change its exit
  code: it says so once, its record says `events_incomplete: true`, and
  the events after are written again. A record that cannot be written
  makes the run exit 1 after an error event, while its last event keeps
  the outcome of what it did, which is not done again. History survives a
  crash of OpsTruss, not a power loss: records and events are not synced
  to disk, unlike state.
- `--json` prints every event of a run on a line of its own, with `v`,
  `seq`, `time`, `run`, `type`, `level`, and `message`, and the address,
  task, step, or stream it belongs to. The last event, of type `run`,
  carries the outcome, the exit code, and the outputs of a task.
- A run that a crash left unfinished is marked cancelled, as interrupted,
  by the next run of its stack as that run ends, also when an older
  OpsTruss removed its lock, and its temporary files go once none of its
  programs is left. A run whose record could not be written is marked as
  its last event says. A run that cannot be marked is a warning of the
  run that tried. A run that fails to start after its record was made,
  also in the daemon, ends failed rather than running.
- Old runs go: a stack keeps 100 runs per task and at most 1 GiB of runs,
  and never drops a run still going or the last run that touched a unit.
  This and marking runs that a crash left read
  `.opstruss/runs/<stack>/.index`, a summary of the finished runs that is
  built again from the runs when it is lost or broken, rather than every
  record: with 10,000 runs in a stack, a run takes about 15 ms more than
  in an empty one, where it took a second.
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
  `dotenv`, the formats of `stdin:` too. A stream that an output reads
  goes to the log once the program ends, with the sensitive values of the
  run hidden, and not at all when that output is sensitive. The outputs of
  a unit or task take at most 1 MiB.
- Scripts run through `sh -e`, `bash --noprofile --norc -eo pipefail`,
  `python3 -c`, `node -e`, or `pwsh -NoProfile -NonInteractive -Command`,
  as `shell:` says. A `run` list whose program is a shell builtin, such as
  `export` or `cd`, that is not in `PATH` fails with a hint to write `run`
  as a string.
- Each program runs in its own process group. A cancel or a timeout sends
  `stop.signal` (default `SIGINT`) to the group, waits `stop.grace`
  (default one minute), and kills what is left; processes that a program
  leaves in its group when it exits are stopped the same way. A second
  Ctrl-C or SIGTERM kills the groups at once, also of services that stop
  in a daemon in the foreground, and the run still writes its record and
  pending marks. A run that a signal ended exits with 130 or 143, as a
  shell reports it.
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
