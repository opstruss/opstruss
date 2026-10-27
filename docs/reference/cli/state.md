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
| [`opstruss state list`](./state-list) | List the addresses that have a record: orphans, pending and abandoned attempts, and tombstones too; then the keys of kv |
| [`opstruss state pull`](./state-pull) | Print a key as stored, and its etag on stderr; key is an address without the stack, \_meta, or \_kv/&lt;name&gt; |
| [`opstruss state push`](./state-push) | Replace a key with stdin, under its lock, when it still has the etag of the pull |
| [`opstruss state rm`](./state-rm) | Forget a record without destroying anything, leaving a tombstone; on a tombstone, remove it; or remove a key of kv |

:::
