---
title: opstruss state rm
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss state rm

Forget a record without destroying anything, leaving a tombstone; on a tombstone, remove it; or remove a key of kv.

## Usage

```text
opstruss state rm <address|_kv/<name>> [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--force` | for \_kv/&lt;name&gt;: remove the key whatever it holds now |
| `--if-match string` | for \_kv/&lt;name&gt;: the etag that state pull printed |
| `--tool` | only move the tool's own state to \_forgotten, and keep the record |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
