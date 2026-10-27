---
title: opstruss runs cancel
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss runs cancel

Cancel a run of the stack, as Ctrl-C at its terminal does, and wait until it ends.

## Usage

```text
opstruss runs cancel <run> [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--force` | kill the programs of the run at once, without the rest of their stop.grace, as a second Ctrl-C does |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
