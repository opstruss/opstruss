---
title: opstruss up
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss up

Bring the units up: each runs when its definition changed.

With -d, up runs in the daemon of the stack, which starts in the background
when none runs and stays until opstruss stop. While a daemon runs, the runs
of the command line go to it. Without one, an up that starts a service makes
this process the daemon of the stack, which shows the output of its services
and stays while they run; Ctrl-C stops them.

## Usage

```text
opstruss up [unit]... [flags]
```

## Flags

| Flag | Description |
|---|---|
| `-d` | run in the daemon of the stack, which starts in the background when none runs |
| `--force` | run the units named although unchanged |
| `--no-deps` | leave out the units that the ones named depend on, or for destroy those that depend on them |
| `-y`, `--yes` | approve the changes of this run, as in CI |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
