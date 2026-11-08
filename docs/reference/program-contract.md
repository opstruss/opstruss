# Program contract

Every program that OpsTruss runs, from a unit, a task, a step, or a
plugin, gets its inputs and gives its results the same way. This page is
what a script or a plugin can rely on.

## In

| What | Holds |
|---|---|
| `INPUT_<NAME>` | Each input of the task or plugin, in uppercase with `-` as `_`; a list or a map as JSON. An input over 64 KiB is only in `OPSTRUSS_INPUTS` |
| `OPSTRUSS_INPUTS` | A JSON file with every input in its own type; mode `0600`, removed after the call |
| `OPSTRUSS_STATE` | For a program of a unit: a JSON file with the last outputs and inputs that succeeded, and an attempt that did not finish; for `destroy`, those of what it takes away ([below](#the-state-file)) |
| stdin | What `stdin:` writes |
| arguments | For the `exec` hook only: what follows `--` in `opstruss exec` |

`INPUT_*` reaches the program that is the body of a task or a plugin. A
`run` inside `steps:` gets values through `env:`, so each value reaches
only the program that names it.

## The state file

`OPSTRUSS_STATE` is a JSON object, with mode `0600`, removed after the
call. A reader ignores the fields that it does not know, since a later
version may add some.

| Field | Holds |
|---|---|
| `inputs`, `outputs` | The inputs and outputs of the last `up` that succeeded; for `destroy`, those of what it takes away: the last `up`, an attempt that did not finish, or an abandoned one |
| `pending` | The attempt that did not finish, when there is one; not for `destroy` |
| `pending.op` | What the attempt ran, such as `up` or `destroy` |
| `pending.inputs`, `pending.outputs` | The inputs of the attempt, and the outputs that it wrote before it stopped |
| `abandoned` | The attempts, with other inputs or another program, that the next `up` destroys once it succeeds; not for `destroy` |
| `abandoned[].op`, `abandoned[].inputs`, `abandoned[].outputs` | As in `pending`, for each attempt |

## Out

| What | Means |
|---|---|
| exit code | 0 is success, anything else failure; a program killed by a signal exits with 128 plus its number, as in a shell |
| `OPSTRUSS_OUTPUT` | A file of outputs: `name=value` lines, `name<<EOF` … `EOF` for several lines, or one JSON object, which keeps types |
| `OPSTRUSS_PLAN` | For `plan` only, optional: the changes, the outputs that change, a digest, and whether the plan is exact |
| `OPSTRUSS_RESULT` | Optional: `{"changed": false, "message": "…", "sensitive": ["<output>"]}`; without `changed`, the program changed something |
| stdout, stderr | The log of the run; also an output with `from: stdout` or `from: stderr` |

```text
image=ghcr.io/acme/app:1.2
notes<<EOF
first line
second line
EOF
```

A text value is read by the `type` of its output, so `port=8080` is a
number for an output of type `integer`. A name written twice keeps the
last value, a line may end in `\r\n`, and a `name<<EOF` block without its
closing `EOF` fails the program.

## Plans

A plugin's `plan` writes `$OPSTRUSS_PLAN`:

```json
{
  "changes": [{"action": "create", "resource": "web", "sensitive": false}],
  "outputs": ["services"],
  "digest": "sha256:…",
  "exact": true
}
```

- `action` is `create`, `update`, `delete`, `replace`, or `run`.
  `protect: true` on the unit refuses a plan with `delete` or `replace`.
- An action that OpsTruss does not know, which a later version may add,
  is a change that needs approval, and `protect: true` refuses it as a
  `replace`. Every action is a lowercase word.
- `digest` is a hash of what will be applied, the actions and their
  values, not of a plan file: the same for the same state and inputs, so
  a plan made again in another run matches. OpsTruss joins it with the
  digest of the definition of the unit.
- `exact: true` is a promise: `up` applies exactly the changes whose
  digest is `$OPSTRUSS_DIGEST`, from a plan it kept in `OPSTRUSS_WORKDIR`
  or `OPSTRUSS_TEMP`, or fails when a new plan differs. A plugin that cannot promise it writes `exact: false`, and
  an approval holds only for an exact plan.
- A plan that writes nothing means that the unit runs.

## Where

| Variable | Is |
|---|---|
| working directory | The directory of the file that writes the unit or task, or its `dir:` |
| `OPSTRUSS_PLUGIN_DIR` | The directory of the plugin; for programs of plugins only |
| `OPSTRUSS_WORKDIR` | The work directory of the unit, used under its lock; a cache that may be empty at any call, as in CI, except between `plan` and `up` of the same turn |
| `OPSTRUSS_ARTIFACTS` | A directory for files that the record of the run keeps |
| `OPSTRUSS_TEMP` | The temporary directory of the run, shared by its programs; removed when the run ends |
| `OPSTRUSS_STATE_DIR` | For a plugin with `state: tool` on the local backend: the `tool/` directory of the unit |
| `OPSTRUSS_STATE_KEY` | The lasting identity of a unit, such as `prod/zone.7f3a9c1e`, which stays when the unit moves; empty in the plan of a unit without a record. It holds `/` and, for an instance, `[` and `]`, so a tag that allows fewer characters gets it hashed or encoded |

## Facts

| Variable | Is |
|---|---|
| `OPSTRUSS_PROJECT`, `OPSTRUSS_STACK` | The project and the stack |
| `OPSTRUSS_ADDRESS` | The address of the unit, such as `prod/web` |
| `OPSTRUSS_TASK`, `OPSTRUSS_RUN` | The task, and the id of the run |
| `OPSTRUSS_RUN_PARENTS` | The ids of the runs that started this one, nearest last |
| `OPSTRUSS_REASON` | Why the unit runs: `changed`, `requested`, `forced`, or `interrupted` for one whose last attempt did not finish; a value it does not know reads as `changed` |
| `OPSTRUSS_DIGEST` | The digest of the plan that was approved |
| `OPSTRUSS_SERVICE_ID` | For a program that the daemon keeps running: an id that its children carry too |
| `OPSTRUSS_CONTRACT` | The version of this contract, `1`, which every 1.x version gives to plugins written for 1.0; a later contract, if one comes, goes only to plugins whose `opstruss:` names the version that brought it |

Not every program runs inside a run or for a unit:

- Every program gets the environment below, `OPSTRUSS_CONTRACT`,
  `OPSTRUSS_PROJECT`, and `OPSTRUSS_STACK`.
- A program that ends inside a run, as a task, a step, a `run` unit, or a
  secret source, also gets `OPSTRUSS_RUN`, `OPSTRUSS_RUN_PARENTS`,
  `OPSTRUSS_TASK`, `OPSTRUSS_REASON`, `OPSTRUSS_TEMP`, the contract files,
  and `OPSTRUSS_ARTIFACTS`, except a secret source.
- A program of a unit gets `OPSTRUSS_ADDRESS` and `OPSTRUSS_STATE_KEY`;
  one under the lock of the unit, as `plan`, `up`, `destroy`, and the
  `exec` hook, also gets `OPSTRUSS_WORKDIR`, `OPSTRUSS_STATE`, and
  `OPSTRUSS_STATE_DIR`. The `status` and `logs` tasks of a plugin run
  without the lock, so they get `OPSTRUSS_STATE` and `OPSTRUSS_STATE_DIR`
  to read, without `OPSTRUSS_WORKDIR`.
- A program that lives outside a run, as a service and the source of a
  `watch`, gets inputs through `INPUT_*` only, and `OPSTRUSS_SERVICE_ID`.

## Environment

A program starts from a clean environment: `PATH`, with the binaries of
the plugin first, `HOME`, `USER`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `TERM`,
the proxy variables, `SSL_CERT_*`, and `SSH_AUTH_SOCK`. To that come the
variables that `env:` sets, and those of the environment of OpsTruss that
`pass:` lets through; for a plugin, only those that its `env:` asks for,
by name or by pattern. `OPSTRUSS_*` and `INPUT_*` come only from
OpsTruss: `env:` cannot set them and `pass:` never passes them.

The output of a program is a pipe, not a terminal, so most tools leave
out color. OpsTruss sets `NO_COLOR=1` when the output of the run has no
color, or when an output reads the stream of the program; a tool that
colors with a flag, as `tofu`, can color when `NO_COLOR` is not set.
`env: {FORCE_COLOR: "1"}` asks a tool for color.

## No keyboard

A program gets no keyboard: its stdin is what `stdin:` writes, or
nothing. At a terminal, a program that asks at `/dev/tty` itself, as
`ssh`, `gpg`, or `chezmoi init` can, shows its question there but never
gets the answer, so its unit runs until its `timeout`, 6 hours by
default, or Ctrl-C stops it. Without a terminal, in CI, in the daemon,
or for a trigger, opening `/dev/tty` fails.

Give a program its answers before it starts: an input, which a run at a
terminal asks for when it is required and has no value, a `sensitive`
one for a secret; `stdin:`; or `env:`, with the flags of the tool that
take them, as `--password-stdin` or `chezmoi init --promptString`.

## A terminal of its own

A program with `tty: true` gets a terminal of its own while the run shows
on one: `opstruss` in the foreground, with its output on a terminal of at
least 22 × 5, without `--json`, and `TERM` other than `dumb`. Tools such
as `docker compose` then draw their color and progress as they do at a
terminal. Elsewhere, in CI, in the daemon, for a trigger, with `-d` or
`--json`, or when the output goes to a pipe or a file, the program gets
pipes, so it must work with either. A program whose stream an output
reads, and the program of a secret, always gets pipes.

- Its stdout and stderr are one terminal, as with `docker run -t`, so its
  stderr comes in the stream `stdout`, and the output `stdout` of a step
  holds the last bytes of that terminal; `stderr` is empty.
- The terminal is as wide as the terminal of the run less 2 columns, the
  gutter, and 2 rows shorter. Its size stays as it was when the program
  started.
- Its stdin is what `stdin:` gives, or nothing, as without `tty`, and
  `/dev/tty` is still the terminal of the operator, which gives it [no
  keyboard](#no-keyboard) either.
- The log gets the rows of that terminal, not each frame that the program
  drew: a row goes to the log once it leaves the top of the terminal, and
  the rows left go once the program ends. Until then, the rows that the
  program may still change show at the bottom of the terminal of the run.

`tty` is a boolean in a unit, a task, a step, a block, and a task of a
plugin; the one nearest to the program wins, and a unit or a step that
calls a task wins over the `tty` of that task. A service, a `task:` step,
and the hooks `watch` and `exec` do not take it.

## Stopping

At cancel or timeout, `stop.signal` (SIGINT by default) goes to the
process group of the program; after `stop.grace` (1m by default) the
group is killed. Processes left in the group when the program ends, such
as `server &` in a script, are stopped the same way before the lock of
the unit is let go.

`stop` of a unit covers each program run for it, the `plan`, `status`,
`logs`, and `destroy` of its plugin included, and `stop` of a task covers
the programs of its steps; `always:` gets its `stop.grace` after a
cancel. Each key comes from the nearest that writes it: a unit or a task
wins over the task it runs, a unit of a component over the unit that uses
the component, and the `stop` of a task in a plugin is the default, so
`stop: {grace: 5m}` on a unit keeps the signal that the plugin asks for.

The lock of the run is the inherited file descriptor 3, which a shell and
Go pass on to children; a run that starts after OpsTruss died waits while
a program of the old run still holds it, or still carries
`OPSTRUSS_RUN=<id>` in its environment.

## As root

A program with `sudo: true` runs as root through `sudo -n -- opstruss
__sudo`, which gets the program and its descriptors, the lock of the run
among them, from OpsTruss, and stops it when OpsTruss stops it or dies. It
gets the contract above, with `HOME` and `USER` of root, unless `env:`
sets them, and `SUDO_USER`, `SUDO_UID`, and `SUDO_GID`; files that it makes
in `OPSTRUSS_TEMP`, `OPSTRUSS_ARTIFACTS`, `OPSTRUSS_WORKDIR`, and
`OPSTRUSS_STATE_DIR` go back to the operator once it ends, except a file
with another link and what is on another filesystem. See
[Run as root](../guides/run-as-root.md).
