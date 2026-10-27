---
title: opstruss plugins
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss plugins

List project sources and their pins without fetching.

## Usage

```text
opstruss plugins
```

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
| [`opstruss plugins fetch`](./plugins-fetch) | Fetch the pinned sources and binaries |
| [`opstruss plugins update`](./plugins-update) | Update the pins of all sources, or of one |
| [`opstruss plugins prune`](./plugins-prune) | Remove cache entries unused for 30 days |

:::
