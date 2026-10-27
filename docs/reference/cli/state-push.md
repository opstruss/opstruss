---
title: opstruss state push
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss state push

Replace a key with stdin, under its lock, when it still has the etag of the pull.

## Usage

```text
opstruss state push <key> [flags]
```

## Flags

| Flag | Description |
|---|---|
| `--force` | replace the key whatever it holds now |
| `--if-match string` | the etag that state pull printed |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
