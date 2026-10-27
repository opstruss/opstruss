# Changelog

Every change to OpsTruss that an operator, or the author of a component or
a plugin, can notice is written down here, newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
with one addition: a release that asks something of a project, a component,
or a plugin says so first, under "Upgrade notes". OpsTruss follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0.0 a
minor release can change what a project, a component, or a plugin relies on.

## [Unreleased]

The first release of OpsTruss: one binary that runs any tool as the units
and tasks of a project, and hands the typed results of one to the next,
on one machine.

### Added

- Projects in `opstruss.yaml` with include files and stacks, each stack
  with its inputs, its state, and `approval: ask` or `auto`.
- Units that run once (`run`), keep running (`service`, with `ready`), or
  use a plugin, a task, or a component; `for_each`, `protect`, `moved:`.
- Tasks with typed inputs as flags, steps, `rescue`/`always`, `retry`,
  `${{ }}` expressions in CEL, and built-in tasks such as `http` and `kv`.
- `plan`, `up`, and `destroy` in graph order under a lock per unit: a
  unit runs again only when it changed, after one approval per run.
- Plugins and components from a path or `github.com/opstruss/catalog`,
  pinned in `opstruss.lock`, and the program contract for plugins.
- Triggers that the daemon of a stack runs: `schedule`, `every`, `watch`,
  `webhook`, and `after`; `up -d` starts it and `stop` stops it.
- Secrets from sops, the environment, a program, or a plugin, hidden in
  every output, and sensitive state encrypted with a data key per stack.
- Runs recorded in `.opstruss/` and read by `runs`, `logs`, and
  `status`; `runs cancel`, `runs rerun`, and `state` repair them.
- `validate`, `explain`, `doctor`, `list`, `outputs`, `exec`, `init`,
  `plugins`, and shell completion; a schema for editors.
- Output as blocks at a terminal, groups in GitHub Actions, and JSON
  events with `--json`.
- Release builds for Linux and macOS on amd64 and arm64, with checksums,
  SBOMs, and build provenance; documentation at opstruss.github.io.

[Unreleased]: https://github.com/opstruss/opstruss/commits/main
