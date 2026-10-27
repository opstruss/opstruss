---
title: opstruss logs
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss logs

Show the output of the units: the log of a service, the logs task of their plugin, or the last run that touched them; and of tasks, from their last run.

## Usage

```text
opstruss logs [unit|task]... [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--follow` | go on with what services, plugins, and runs of the tasks write, until Ctrl-C; -f is the same |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
