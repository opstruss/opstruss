---
title: opstruss destroy
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss destroy

Destroy the units with the destroy contract of their records.

## Usage

```text
opstruss destroy [unit]... [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--force` | run destroy although pass values differ from those recorded |
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
