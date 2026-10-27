---
title: opstruss outputs
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss outputs

Print the outputs of the project from the stored outputs of its units.

## Usage

```text
opstruss outputs [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--show-sensitive` | print sensitive outputs as they are |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
