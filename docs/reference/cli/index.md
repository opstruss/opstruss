---
title: opstruss
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss

Event-driven workflow orchestration that runs any tool and hands its typed results to the next..

## Usage

```text
opstruss [flags]
```

## Flags

| Flag | Description |
|---|---|
| `-C dir` | run as if OpsTruss started in dir; a -C after another is relative to it |
| `--completion string` | print a completion script for bash, zsh, or fish |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |
| `-v`, `--version` | print the version, as opstruss version does |

## Commands

| Command | Description |
|---|---|
| [`opstruss init`](./init) | Set up a project in this directory |
| [`opstruss validate`](./validate) | Check the project's files without running anything |
| [`opstruss plan`](./plan) | Show what up would do and why, without changing anything |
| [`opstruss up`](./up) | Bring the units up: each runs when its definition changed |
| [`opstruss destroy`](./destroy) | Take the units of the stack down |
| [`opstruss status`](./status) | Show the state of the units |
| [`opstruss logs`](./logs) | Show the output of units and tasks |
| [`opstruss list`](./list) | List the tasks and units of the project |
| [`opstruss outputs`](./outputs) | Print the outputs of the project |
| [`opstruss runs`](./runs) | List the runs of the stack, or show one |
| [`opstruss explain`](./explain) | Print the project of a stack as resolved |
| [`opstruss doctor`](./doctor) | Check what runs of the stack need on this machine |
| [`opstruss exec`](./exec) | Run a program with the environment of a unit |
| [`opstruss plugins`](./plugins) | List the sources of the project and their pins |
| [`opstruss state`](./state) | Read and repair the records of a stack |
| [`opstruss stop`](./stop) | Stop the daemon of the stack and its services |
| [`opstruss unlock`](./unlock) | Forget a leftover service so up can start it again |
| [`opstruss version`](./version) | Print the version |
| [`opstruss run`](./run) | Run a task of the project by name |

:::
