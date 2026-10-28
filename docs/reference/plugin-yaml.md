---
title: opstruss.plugin.yaml
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss.plugin.yaml

An OpsTruss plugin.

Keys that start with x- are left for your own use.

| Key | Type | Description |
|---|---|---|
| `name` (required) | string | The name of the plugin: lowercase letters, digits, - and \_, starting with a letter. |
| `description` | string | What the plugin does. |
| `opstruss` | string | The oldest OpsTruss version that reads this file, as MAJOR.MINOR. |
| `requires` | list of string | Programs that must be in PATH for every task of the plugin. |
| `env` | list of string | Variables the plugin asks for, by name or by pattern as in pass:; the project grants them with pass:. |
| `state` | one of `opstruss`, `tool` | Where the state of a unit is: opstruss, the outputs of up in its record, or tool, the files of the tool in the tool directory of the unit; opstruss by default. |
| `inputs` | map of [Input](opstruss-yaml#input) | Inputs of the units that use the plugin, which their with: gives, read by every task. |
| `outputs` | `tool` or map of [Output](opstruss-yaml#output) | Outputs that up writes, or tool when the code of the user in the tool names them. |
| `tasks` | map of [Task](opstruss-yaml#task) | The tasks of the plugin: the standard tasks plan, up, destroy, status, and logs, hooks such as exec, and tasks of its own. |
| `bin` | map of map of [bin](#bin) | Binaries that OpsTruss downloads for the plugin, by name and platform, such as linux/amd64 or darwin/arm64, which its programs find in PATH; OpsTruss reads only the platform it runs on. |
| `backend` | [backend](#backend) | A backend that keeps the state of stacks; not supported yet. |

## bin {#bin}

Binaries that OpsTruss downloads for the plugin, by name and platform, such as linux/amd64 or darwin/arm64, which its programs find in PATH; OpsTruss reads only the platform it runs on.

| Key | Type | Description |
|---|---|---|
| `url` (required) | string | The https URL of the binary, or of a .tar.gz archive that holds it at its root. |
| `sha256` (required) | string | The SHA-256 of the file at url. |

## backend {#backend}

A backend that keeps the state of stacks; not supported yet.

| Key | Type | Description |
|---|---|---|
| `run` (required) | string or list of string | The program of the backend. |

:::
