# Exit codes and JSON

## Exit codes

| Code | Means |
|---|---|
| 0 | Success. Also a command that found nothing to read, such as `plan` in a project without units, which says so on stderr |
| 1 | Failure: a unit or task failed, the files are not valid, a unit cannot be planned, or the record of the run could not be written |
| 2 | Stopped at the plan: changes need approval and no terminal answers, as in CI without `--yes` |
| 130, 143 | Stopped by SIGINT (Ctrl-C) or SIGTERM |

- 1 wins over 2: a run where one unit failed and another waits for
  approval exits with 1.
- `plan` exits with 1 when a unit cannot be planned, and with 0 when a
  unit is held by another run.
- While a unit cannot be planned, `up` still runs the other units and
  exits with 1, but destroys and forgets nothing: the instances that left
  `if` or `for_each` and the units that `removed:` decides end `skipped`
  with `skipped: waits until every unit can be planned`, and go at the
  first `up` that plans every unit.
- `doctor` exits with 1 only when a check is `error`, which is what fails
  a run. A later version may add checks, so it fails only where a run
  would.
- A run that a signal stops ends `cancelled`: its record and its last
  event have `reason` `signal` and the same `exit_code` as the process. A
  run that ended before the signal stopped anything keeps its outcome.
- A run that `opstruss runs cancel` cancels exits with 1, since it got no
  signal; `runs cancel` itself exits with 0 once that run ends as
  `cancelled`.
- A run whose process was killed, such as by SIGKILL, ends with no last
  event. The next run of its stack marks its record `cancelled`, with
  `reason` `interrupted` and `exit_code` 1, and says so on stderr.
- `logs -f` and `runs <id> -f` stopped by Ctrl-C exit with 0, since they
  only read, and so does a daemon stopped by a signal.

## JSON

With `--json`, a command that runs something, such as `plan`, `up`,
`destroy`, or a task, prints one event per line. A command that reads, such as `outputs`, `list`, `explain`,
`doctor`, `runs`, and `state list`, prints one JSON document.

Four fields keep the values that this page lists: `outcome`, the
`status` of a run and its events (`running` or `finished`), `level`, and
`stream`. A later version may add a value to any other field with a list
of values, such as `type`, `action`, `reason`, `approval`, `kind`, and the
`status` of a check of `doctor`, so a reader, and an expression, takes a
value that it does not know as another case: a `reason` it does not know
is a run for another reason.

An event:

```json
{"version":1,"seq":12,"time":"2026-10-19T03:21:07.685Z","run":"20261019T032107Z-4b2e91d0","type":"log","level":"info","address":"prod/web","task":"up","step":"build","stream":"stdout","message":"added 312 packages"}
```

| Field | Is |
|---|---|
| `version` | The version of the shape of events, `1` |
| `seq` | The order within the run, since two events can have the same `time` |
| `time` | When, in RFC 3339 and UTC |
| `run` | The id of the run |
| `type` | `run`, `plan`, `approval`, `unit`, `task`, `step`, `log`, or `diagnostic`; from the daemon also `daemon`, `service`, and `trigger` |
| `level` | `debug`, `info`, `warn`, or `error` |
| `address`, `task`, `step` | What the event is about, when that applies |
| `stream` | `stdout` or `stderr`, for a line of a program |
| `message` | Text for people, with secrets hidden |
| `status` | `running` on the first event of a run, a unit operation, a task, or a step, and `finished` on the last |
| `outcome` | On a `finished` event: `success`, `failure`, `skipped`, or `cancelled` |
| `exit_code` | On the last event of a run: the exit code of the command |
| `outputs` | On the last event of a run: the outputs of the task, each an output as [below](#documents); none for a task with `for_each` ([tasks](../guides/tasks#for-each-item)) |
| `plan` | On a `plan` event: the plan of its unit, as `units.<name>.plan` reads it (below) |
| `sudo` | `true` on the `debug` `diagnostic` event that a program which runs as root starts with |

- A reader ignores the fields and types it does not know: a new version
  may add some, and `message` is there to show any event.
- The last event of a run is its `run` event with `outcome`, `exit_code`,
  and `outputs`, so a script that runs `opstruss <task> --json` reads the
  result from one line:

  ```sh
  opstruss build --json | tail -n 1 | jq -r .outputs.image.value
  ```

- A unit that is not run, such as one that is unchanged, gets only its
  last event.
- Secrets and sensitive values are hidden in every event, as they are at
  the terminal.
- What fails before a run starts, such as a file that is not valid or a
  stack that the project does not have, is JSON too: a command that
  prints events prints a `diagnostic` event for each problem, with its
  file and line in `message`, and a last `run` event with `exit_code`; a
  command that prints a document prints `{"error": "<message>"}`. stderr
  has the same as text.

A plan, in a `plan` event and in `units.<name>.plan`:

| Field | Is |
|---|---|
| `changes` | What the plan of its plugin changes, each with `action` (`create`, `update`, `delete`, `replace`, or `run`; `forget` for a unit that `removed:` drops from the state without destroying it) and `resource` when it names one |
| `outputs` | The outputs that the plan of its plugin says change |
| `exact` | Whether `up` gets this plan: `false` for a unit planned at its turn, or a plugin that does not promise one |
| `digest` | What an approval of the plan approves; `""` for a unit planned at its turn |
| `summary` | The plan line, as without `--json` |
| `approval` | `auto` when the unit runs without a question |
| `reason` | Why it runs: `new`, `pending`, `forced`, `context`, `definition`, `not_running`, `generates`, or `plan`; for a destroy, `requested`, `gone`, or `removed` |
| `changed` | The parts of its definition that changed, such as `sources` |
| `after` | The addresses of the units whose outputs this run changes first and the unit reads, in the order of the plan; it is planned at its turn |
| `reads` | The parts of the unit that read those outputs, such as `env.URL` |

The record of each run keeps the same events, which
`opstruss runs <id> --json` prints again. In a `diagnostic` of what fails
before a run starts, `run` is empty, since there is no run.

## Documents

A command that reads prints one JSON object. A later version may add
fields to it, never remove one or change what it means. An empty list is
`[]`, never `null`, and times are RFC 3339 in UTC; those of runs and
events are to the millisecond.

| Command | Fields |
|---|---|
| `list` | `stack`; `tasks`, each with `name`, `description`, `internal`, `triggers`, `next`, and of its last run `run`, `via`, `outcome`, and `ended`; `units`, each with `address`, `kind`, `state` (`up`, `pending`, `absent`, or the state of a service), `run`, and `outputs`; `triggers`, with `daemon` and `error`, when a task has triggers |
| `runs` | `stack`; `runs`, newest first, each a record of a run (below): the 20 newest, or as many as `--limit` says, and every one with `--limit 0` |
| `outputs` | `stack`; `outputs`, by name |
| `doctor` | `stack`; `checks`, each with `check`, `status`, `message`, and when they apply `name`, `path`, and `for`, what needs it |
| `explain` | `project`, `stack`, `inputs` (each with its `value` and where it is `from`), `order`, and `units`, `tasks`, `outputs`, and the other keys of the project, as its files have them with the stack applied |
| `state list` | `stack`; `records`, each with `address`, `id`, and `kind`; `kv` |
| `uses`, `uses fetch`, `uses update` | `sources` |
| `uses prune` | `removed` |
| `validate` | `valid`, `problems`, and `warnings`, each problem and warning with `file`, `line`, `column`, `path`, and `message`; `line` and `column` start at 1, and `column` counts characters, not bytes; `file` is relative to the project, or `<repository>@<ref>/<path>` for a file of a remote plugin, component, or catalog; only problems make `valid` false |
| `stop` | `stack`, `stopped` |
| `init` | `wrote`, the files it wrote, and `sources`, the pins it made ready |
| `version` | `version`, `os`, `arch` |
| A command that fails before it reads | `error` |

An output, in `outputs`, in the units of `list`, and in the last event and
the record of a run:

```json
{"value": "https://example.com", "sensitive": false, "type": "string"}
```

- `value` is missing from a sensitive output, unless `outputs
  --show-sensitive` shows it.
- `type` is the type that the unit, its plugin, or the task declares, when
  it does.

A record of a run has `version`; `opstruss`, the version of OpsTruss that
ran it; `run`; `parent`, the run it ran inside; `project`, `stack`,
`command`, `task`, and `args`; `trigger`, what made it, and `simulated`
for `--event`; `user`, `host`, `hostname`, and `pid`; `started` and
`ended`; `status`, `running` or `finished`, and once it is `finished`,
`outcome`, `reason` (why it ended `cancelled`: `signal`, `requested` by
`opstruss runs cancel`, `concurrency` for a call with `concurrency:
cancel`, or `interrupted` when its process died), and `exit_code`; `units`
and `tasks`, what ran inside it; `outputs`, those of its task;
`events_incomplete` when its events could not all be written; `no_deps`;
`rerun_of`; and `replay`, what `runs rerun` needs.

## Color

`NO_COLOR` that is not empty turns color off; `FORCE_COLOR` or
`CLICOLOR_FORCE` that is not empty or `0` turns it on; `TERM=dumb` turns
it off; otherwise there is color at a terminal and in GitHub Actions.
stdout and stderr are decided apart. With `--json`, only `FORCE_COLOR`
and `CLICOLOR_FORCE` turn it on.
