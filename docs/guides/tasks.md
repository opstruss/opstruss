# Tasks and steps

A task is a command of the project: `opstruss <task>` runs it, its
inputs are flags, and `opstruss --help` lists it under "Project tasks". A
task runs one program with `run`, or `steps` that call programs, other
tasks, built-in tasks, and `up` itself.

<<< @/examples/tasks/opstruss.yaml

```console
$ opstruss release --version 1.2.0
▸ test
│ tests pass
✓ test  0.0s

  + site  new

Plan of stack default: 1 to run

Apply site? [y/N] y
✓ site › up: done  0.2s
✓ release › up  0.7s
▸ release › tag
│ tagged v1.2.0
✓ release › tag  0.0s
▸ release › notify
│ release 1.2.0 ended
✓ release › notify  0.0s
✓ release › step 1  0.7s
✓ release  0.7s

Up of stack default: 1 done
```

## Inputs

Each input is a flag: `--version 1.2.0`. An input that is `required`
and not given is asked for at a terminal, and without one the task
stops with the flag to pass. A string, number, boolean, list, or object
is checked against its type before anything runs, and reaches a program
as `INPUT_<NAME>`, or as `${{ inputs.<name> }}` in `env:` and in a `run`
list.

## Steps

- `run` runs a program, `task` a task of the project or the standard
  tasks `up`, `destroy`, `status`, and `logs` with `units:`, and a key
  such as `http:` or `kv:` a [built-in task](../reference/builtin-tasks).
- `id` names a step, whose outputs a later step reads as
  `steps.<id>.outputs.<name>`.
- `if` skips a step, `retry` runs it again, `timeout` stops it, and
  `continue_on_error` goes on after it fails. A step that a timeout or a
  cancel stops fails, also when its program exits 0 once stopped; once
  the time of the task runs out or the run is cancelled, no other step
  starts, except those of `always:`.
- `needs` runs other tasks first, once per run.
- `shell`, `dir`, `env`, `pass`, and `tty` of a task or a `block` are the
  defaults of the steps in it; a step writes its own to change one.

A tool that draws progress at a terminal, as `npm install` does, shows it
with `tty: true` while the run shows on a terminal; in CI, and with
`--json`, it prints lines as before
([program contract](../reference/program-contract#a-terminal-of-its-own)):

```yaml
tasks:
  setup:
    tty: true
    steps:
      - run: npm install
      - run: npm test
        tty: false        # plain lines, as a test reporter prints them
```

## Approval is a step

`task: up` in a step plans and asks before it changes anything, as
`opstruss up` does: at a terminal it asks, and without one it stops at
the plan, which fails the step. `opstruss release --yes`, or `yes: true`
on the step, approves the changes of that run. A step that asks a person
in another way, such as a message in a chat that waits for an answer, is
a task of its own that the step calls.

## When a step fails

`block` groups steps; `rescue` runs when one of them fails, and `always`
runs after both, whatever happened.

- A `rescue` that succeeds handles the failure, and the task succeeds.
  To report a failure and still fail, end the rescue with a step that
  fails, as the `assert` above does.
- `always` is the place for what must run in every case, such as a
  notification; `if:` on its steps reads what happened.

## Running a task

| Command | Does |
|---|---|
| `opstruss <task> --<input> <value>` | Runs the task with its inputs |
| `opstruss run <task>` | The same, also for a task whose name a command of OpsTruss takes |
| `opstruss <task> -d` | Runs it in the daemon of the stack |
| `opstruss list` | Lists each task with its last run |
| `opstruss logs <task>` | Shows the output of its last run |
| `opstruss runs rerun <run>` | Runs it again with the inputs of that run |

A task with `on:` also runs by itself: [Triggers and the daemon](./triggers).
