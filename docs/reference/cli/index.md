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
| `-C dir` | run as if OpsTruss started in dir, like git -C; a -C after another is relative to it |
| `--completion string` | print a completion script for bash, zsh, or fish |
| `--input stringArray` | set a project input, as name=value |
| `--json` | print JSON |
| `--locked` | require existing dependency pins without changing opstruss.lock |
| `--stack string` | the stack to use; defaults to the only one, or $OPSTRUSS\_STACK |

## Commands

| Command | Description |
|---|---|
| [`opstruss destroy`](./destroy) | Destroy the units with the destroy contract of their records |
| [`opstruss doctor`](./doctor) | Check that this machine has what a run of the stack needs, without running it |
| [`opstruss exec`](./exec) | Run a program with what the programs of one unit get, under its lock, on this terminal |
| [`opstruss explain`](./explain) | Print the project of one stack as read and resolved, without state |
| [`opstruss init`](./init) | Set up the project in this directory: opstruss.yaml and .gitignore |
| [`opstruss list`](./list) | List the tasks of the project with their last run, and the units with their state |
| [`opstruss logs`](./logs) | Show the output of the units: the log of a service, the logs task of their plugin, or the last run that touched them; and of tasks, from their last run |
| [`opstruss outputs`](./outputs) | Print the outputs of the project from the stored outputs of its units |
| [`opstruss plan`](./plan) | Show what up would do and why, without changing anything |
| [`opstruss plugins`](./plugins) | List project sources and their pins without fetching |
| [`opstruss run`](./run) | Run a task of the project, also one that a built-in command shadows |
| [`opstruss runs`](./runs) | List the runs of the stack, or show the events of one as it ran |
| [`opstruss state`](./state) | Read and repair the state of a stack without touching its storage |
| [`opstruss status`](./status) | Show the state of the units: the status task of their plugin, or what their record says |
| [`opstruss stop`](./stop) | Stop the daemon of the stack, which cancels the runs in it and stops its services |
| [`opstruss unlock`](./unlock) | Forget what is left of a service that cannot be proven its own, so up starts it again |
| [`opstruss up`](./up) | Bring the units up: each runs when its definition changed |
| [`opstruss validate`](./validate) | Check the project's files without running anything |
| [`opstruss version`](./version) | Print the version |

:::
