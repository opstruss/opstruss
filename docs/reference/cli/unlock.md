---
title: opstruss unlock
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss unlock

Release the lock of a unit that a plugin backend keeps after its holder died, as the holder's token: one whose holder ran on this host and no longer runs at once, one of another host once you agree, or with --yes. \_daemon names the lease of the daemon of the stack, \_kv the lock of kv, and \_meta that of \_meta. For a unit, also forget what is left of its service that cannot be proven its own, so up starts it again.

## Usage

```text
opstruss unlock <unit> [flags]
```

## Examples

```sh
opstruss unlock web --stack prod
opstruss unlock _daemon --stack prod --yes
```

## Flags

| Flag | Description |
|---|---|
| `--global` | use the global units, one for every stack, rather than a stack |
| `-y`, `--yes` | release a lock that a host other than this one holds without asking, as in CI |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
