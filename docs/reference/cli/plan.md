---
title: opstruss plan
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss plan

Show what up would do and why, without changing anything.

## Usage

```text
opstruss plan [unit]... [flags]
```

## Examples

```sh
opstruss plan
opstruss plan --stack prod --json
```

## Flags

| Flag | Description |
|---|---|
| `--no-deps` | leave out the units that the ones named depend on, or for destroy those that depend on them |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
