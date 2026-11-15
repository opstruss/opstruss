---
title: opstruss uses fetch
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss uses fetch

Fetch the sources of the project and the binaries of their plugins at the commits that opstruss.lock pins, pinning a source that has no pin yet; what the cache lost or holds broken is fetched again.

## Usage

```text
opstruss uses fetch
```

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
