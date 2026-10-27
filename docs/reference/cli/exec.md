---
title: opstruss exec
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss exec

Run a program with what the programs of one unit get, under its lock, on this terminal.

## Usage

```text
opstruss exec <unit> -- <program> [arg]...
```

## Examples

```sh
opstruss exec infra -- tofu state list
opstruss exec web -- docker compose ps
```

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
