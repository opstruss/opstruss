# Troubleshooting

`opstruss doctor` checks what a run of the stack needs on this machine:
the programs of each unit and task, the age key, sops, and the
filesystem of `.opstruss/`. Start there.

## A program is not in PATH

```text
default/infra: error: plugin cat.tofu requires jq, which is not in PATH
```

Install the program, for example with `mise use jq`, or put the
directory that has it in `PATH`. `opstruss doctor` shows where each
program is found. The [Install](./getting-started/install#requirements)
page lists what each plugin needs.

## A unit is pending after a crash

When OpsTruss is killed during a run, the record of a unit that was
running keeps a `pending` mark. The next run waits for the programs of
the old run to end and runs the unit again; `status` shows the mark until
then. A run that cannot list processes, as in some sandboxes, cannot
wait, so the unit fails until it runs where it can.

## Another run holds a lock

```text
default/web: waiting for the lock held by run 20261027T124725Z-0069f2ad (pid 4121 on build-1, for 12s)
```

Each unit has a lock, so two runs never change it at once. Let the other
run end, or cancel it with `opstruss runs cancel <run>`.

## context changed

```text
default/dns: error: context changed: AWS_PROFILE differs from the record, whose destroy reaches the old target; up --force moves the unit and forgets that target
```

A variable that `pass:` lets through, such as `AWS_PROFILE`, differs from
the one the unit was made with. Set it back, or run `up --force` to move
the unit to the new target. A credential that changes every session
belongs in `secrets:` instead: [Secrets with sops](./guides/secrets#variables-for-the-tools-in-plugins).

## A trigger does not run

`opstruss list` says when no daemon runs the triggers, and why the last
`up` could not install them, such as a secret that is not set. Start the
daemon with `opstruss up -d`, from a shell with the same environment
each time. The daemon writes what it does to
`.opstruss/logs/<stack>/_daemon.jsonl`.

## The daemon runs an older OpsTruss

After an upgrade, a daemon keeps the binary that started it. Run
`opstruss stop`, then `opstruss up -d`.

## A stack is gone from stacks:

```text
stack default has 3 records, but the project does not declare it; opstruss destroy --stack default takes them down
```

A stack keeps its records after the project stops declaring it.
`opstruss destroy --stack default` takes its units down from the records;
`opstruss state list --stack default` shows them.

## Still stuck

`opstruss runs <run>` shows every event of a run, and `OPSTRUSS_LOG=debug`
adds the diagnostics of the engine. Search the
[issues](https://github.com/opstruss/opstruss/issues), or open one with
the output of `opstruss version` and the smallest `opstruss.yaml` that
shows the problem, without secrets.
