---
title: opstruss uses
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss uses

List project sources and their pins without fetching.

## Usage

```text
opstruss uses
```

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

## Commands

| Command | Description |
|---|---|
| [`opstruss uses fetch`](./uses-fetch) | Fetch the pinned sources and binaries |
| [`opstruss uses update`](./uses-update) | Update the pins of all sources, or of one |
| [`opstruss uses prune`](./uses-prune) | Remove cache entries unused for 30 days |

:::
