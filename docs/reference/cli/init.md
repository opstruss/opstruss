---
title: opstruss init
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss init

Set up the project in this directory, adding only what is missing: opstruss.yaml, whose first line names its schema for editors, and .opstruss/ in .gitignore. Then check every stack as validate does, and fetch what the project uses, with the binaries for this machine, as uses fetch does, so that a fresh clone is ready to plan. Running it again is safe. With --stack, also make the state of that stack. The age key of this machine and the data key of a stack are made by the first command that needs them.

## Usage

```text
opstruss init [flags]
```

## Examples

```sh
mkdir shop && cd shop && opstruss init
opstruss init --name shop
git clone https://github.com/example/shop && cd shop && opstruss init
```

## Flags

| Flag | Description |
|---|---|
| `--name string` | the name of a new project; made from the directory's name when not given |

## Global flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

:::
