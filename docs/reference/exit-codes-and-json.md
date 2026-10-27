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
- A run that `opstruss runs cancel` cancels exits with 1, since it got no
  signal; `runs cancel` itself exits with 0 once that run ends as
  `cancelled`.
- `logs -f` and `runs <id> -f` stopped by Ctrl-C exit with 0, since they
  only read, and so does a daemon stopped by a signal.

## JSON

With `--json`, a command that runs something, such as `plan`, `up`,
`destroy`, or a task, prints one event per line. A command that reads, such as `outputs`, `list`, `explain`,
`doctor`, `runs`, and `state list`, prints one JSON document.

An event:

```json
{"v":1,"seq":12,"time":"2026-10-19T03:21:07.685Z","run":"20261019T032107Z-4b2e91d0","type":"log","level":"info","address":"prod/web","task":"up","step":"build","stream":"stdout","message":"added 312 packages"}
```

| Field | Is |
|---|---|
| `v` | The version of the shape of events, `1` |
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
| `outputs` | On the last event of a run: the outputs of the task |
| `plan` | On a `plan` event: the plan of its unit, as `units.<name>.plan` reads it |

- A reader ignores the fields and types it does not know: a new version
  may add some, and `message` is there to show any event.
- The last event of a run is its `run` event with `outcome`, `exit_code`,
  and `outputs`, so a script that runs `opstruss <task> --json` reads the
  result from one line.
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

The record of each run keeps the same events, which
`opstruss runs <id> --json` prints again.

## Color

`NO_COLOR` that is not empty turns color off; `FORCE_COLOR` or
`CLICOLOR_FORCE` that is not empty or `0` turns it on; `TERM=dumb` turns
it off; otherwise there is color at a terminal and in GitHub Actions.
stdout and stderr are decided apart. With `--json`, only `FORCE_COLOR`
and `CLICOLOR_FORCE` turn it on.
