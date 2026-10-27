# How OpsTruss works

## Units, tasks, and the graph

A project declares **units**, things that `up` brings up and `destroy`
takes away, and **tasks**, actions that run when asked or when a trigger
fires. A unit runs a program (`run`), keeps one running (`service`), or
uses a plugin, a task, or a component (`uses`).

What a unit reads, such as `${{ units.db.outputs.host }}`, makes it come
after that unit, as `needs:` does. OpsTruss builds a graph of the units
from those references, runs the ones that do not depend on each other at
once, and hands the typed outputs of one to the next: a number stays a
number, a list stays a list.

## Records and digests

Each unit has a **record** in the state of its stack: the outputs of its
last `up`, the program that destroys it, and a **digest** of its
definition: its program, inputs, environment, and the content of its
`sources`. `plan` compares the digest of the files with the record:

- a unit whose digest is the same is `unchanged`, unless a file it
  `generates` is missing;
- a unit whose digest changed runs, and the plan says which part
  changed;
- a plugin with a `plan` task decides for its unit, as `tofu plan` does.

The record keeps the destroy program and its inputs, so `destroy`, and a
unit that leaves the files, take away what was made, without the
definition. A value that `pass:` let through is kept as a fingerprint:
a destroy that would reach another target, such as another
`AWS_PROFILE`, stops first.

Before a program of a unit runs, its record is marked `pending`. If
OpsTruss is killed, the next run sees the mark, waits for the programs of
the old run to end, and runs the unit again with
`OPSTRUSS_REASON=interrupted`.

## Plan and approval

`up` plans every unit first, shows the plan, and asks once at a terminal
before it changes anything. In CI, `--yes` approves; without it, `up`
stops at the plan and exits with 2. A plan that a plugin marks **exact**
is the one that `up` applies: if the plan changed since the approval, the
unit fails rather than apply something else.

Each address has a lock, so two runs never change one unit at once; a
run waits for a lock that another holds, and says who holds it.

## Stacks and state

A **stack** is a copy of the units with its own inputs and its own
state, such as `dev` and `prod`. This version keeps the state of each
stack in `.opstruss/state/<stack>/` of the project, on the machine that
runs it. Back it up: with the age key of the machine, it is the only
copy.

## Sensitive values

A secret, and anything computed from it, is **sensitive**. OpsTruss
hides it as `***` in every event, so the terminal, logs, records, and
`--json` never show it, and encrypts it in the state with the data key of
the stack. Each value reaches only the programs that name it in `env:`.

## The daemon

Services and triggers need a process that stays: the **daemon** of the
stack, which `up -d` starts. While it runs, the commands of that stack go
to it, so a run from the command line and a run from a trigger share the
same locks and the same view of the services.
