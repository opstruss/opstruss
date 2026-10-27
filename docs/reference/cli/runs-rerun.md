---
title: opstruss runs rerun
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss runs rerun

Run a task, up, or destroy again with the inputs and the event of a run, through the project as it is now; approval is asked again.

## Usage

```text
opstruss runs rerun <run> [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--force` | for up, run the units named although unchanged; for destroy, although pass values differ |
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
