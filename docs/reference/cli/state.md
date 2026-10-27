---
title: opstruss state
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss state

Read and repair the state of a stack without touching its storage.

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
| [`opstruss state list`](./state-list) | List the records and kv keys of the stack |
| [`opstruss state pull`](./state-pull) | Print one record or key as stored |
| [`opstruss state push`](./state-push) | Replace one record or key from stdin |
| [`opstruss state rm`](./state-rm) | Forget a record without destroying anything |

:::
