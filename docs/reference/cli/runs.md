---
title: opstruss runs
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss runs

List the runs of the stack, or show the events of one as it ran.

## Usage

```text
opstruss runs [run] [flags]
```

## Examples

```sh
opstruss runs
opstruss runs 4b2e91d0     # the events of one run, by the end of its id
opstruss runs 4b2e91d0 -f  # follow a run that goes on
```

## Flags

| Flag | Description |
|---|---|
| `--follow` | go on with what a run that still runs writes, until it ends; -f is the same |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

## Commands

| Command | Description |
|---|---|
| [`opstruss runs cancel`](./runs-cancel) | Cancel a run of the stack |
| [`opstruss runs rerun`](./runs-rerun) | Run a task, up, or destroy again as a run did |

:::
