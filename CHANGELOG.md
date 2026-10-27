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

- Projects in `opstruss.yaml`, with include files and stacks: units that
  run once (`run`), keep running (`service`), or use a plugin, a task, or
  a component (`uses`), and tasks with steps, typed inputs and outputs,
  `${{ }}` expressions in CEL, and built-in tasks such as `http` and `kv`.
- `plan`, `up`, and `destroy` in the order of the graph, each unit under
  the lock of its address; a unit runs again only when its definition or
  its `sources` change, and asks for approval at a terminal, once for the
  run, or takes `--yes` in CI.
- Plugins and components from a local path or from
  `github.com/opstruss/catalog`, pinned in `opstruss.lock`, and the
  program contract that plugins are written to.
- Triggers that the daemon of a stack runs: `schedule`, `every`, `watch`,
  `webhook`, and `after`; `up -d` starts the daemon.
- Secrets from sops, the environment, a program, or a plugin, hidden as
  `***` in everything a run writes, and a data key per stack that keeps
  sensitive state encrypted.
- State in `.opstruss/`, with a record and the events of every run, read
  by `runs`, `logs`, and `status`, and changed by `runs cancel`,
  `runs rerun`, `moved:`, and `state`.
- `validate`, `explain`, `doctor`, `list`, `outputs`, `exec`, `init`,
  `plugins`, and shell completion.
- Output as blocks at a terminal, groups in GitHub Actions, and one JSON
  event per line with `--json`.
- Release builds for Linux and macOS on amd64 and arm64, with checksums,
  SBOMs, and build provenance attestations.

[Unreleased]: https://github.com/opstruss/opstruss/commits/main
