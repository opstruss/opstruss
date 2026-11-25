# Changelog

Every change to OpsTruss that an operator, or the author of a component or a plugin, can notice is written down here, newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), with one addition: a release that asks something of a project, a component, or a plugin says so first, under "Upgrade notes". OpsTruss follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html): a minor or patch release keeps every project, component, and plugin working, as [Compatibility](docs/reference/compatibility.md) says, and a change that cannot keep them working comes only in a new major version.

## [Unreleased]

### Upgrade notes

- Tasks with `after:` also run after a command that runs without the daemon, as in CI; the command waits for them and exits with 1 when one fails, and `-d` leaves them to the daemon ([#16](https://github.com/opstruss/opstruss/pull/16)).

### Added

- Global units and tasks: `global: true` makes one instance for the whole project rather than one per stack, which a run of a stack brings up when its units need it, and `--global` chooses them on the command line; needs `opstruss: "1.1"` ([#16](https://github.com/opstruss/opstruss/pull/16)).
- `after: {task: init}` runs a task once `opstruss init` succeeds, so a fresh clone is ready with one command, and `event.stack` names the stack of the run that a task follows ([#16](https://github.com/opstruss/opstruss/pull/16)).
- A key added after 1.0, in a file whose `opstruss:` is older, fails with the version it needs rather than as an unknown key ([#16](https://github.com/opstruss/opstruss/pull/16)).
- Plugin backends: `backend: {uses, with, pass}` keeps the state of the stacks in a plugin, as `s3` of the catalog, so a team and CI share records and locks; `opstruss init` starts a stack there once a probe of the storage passed; needs `opstruss: "1.1"` ([#17](https://github.com/opstruss/opstruss/pull/17)).
- `opstruss unlock <unit>` releases a lock that a dead holder left in a plugin backend, after a question or with `--yes`; the next run takes over one whose holder died on the same machine ([#17](https://github.com/opstruss/opstruss/pull/17)).
- In a plugin backend, only the daemon that holds the lease of a stack installs its triggers and services ([#17](https://github.com/opstruss/opstruss/pull/17)).
- A plugin backend may serve `scan`, which gives the keys of a stack with their data in one operation, so a plan reads each stack at once; the probe checks it, and that `list` shows the locks that are held ([#18](https://github.com/opstruss/opstruss/pull/18)).

### Changed

- `up` neither locks nor reads again a unit whose plan changes nothing, while no unit it needs, directly or through others, ran, so it no longer waits for a lock that another run holds there; a change to the files of such a unit after the plan waits for the next run ([#18](https://github.com/opstruss/opstruss/pull/18)).

### Fixed

- A run that waits for a lock of the local backend, just taken by another, names that run rather than an empty holder ([#17](https://github.com/opstruss/opstruss/pull/17)).
- A `kv` write that cannot remove the names whose `ttl` passed now fails before it changes the key, so a `retry` with `expect` tries the same write again, rather than failing after the key was written ([#19](https://github.com/opstruss/opstruss/pull/19)).
- A webhook with `respond: accepted` now answers 202 with the run also when the run ended before the answer, rather than at times 200 or 500 with its outcome, which made a sender that retries after a 500 run the task again ([#23](https://github.com/opstruss/opstruss/pull/23)).
- The daemon starts also when the project is on a filesystem that cannot hold a Unix socket or change its mode, such as virtiofs, with its socket under `$TMPDIR`, rather than failing `up -d` with `chmod …: invalid argument`.

## [1.0.0] - 2026-11-18

The first release of OpsTruss: one binary that runs any tool as the units and tasks of a project, and hands the typed results of one to the next, on one machine.

### Added

- Projects in `opstruss.yaml` with include files and stacks, each stack with its inputs, its state, and `approval: ask` or `auto`.
- Units that run once (`run`), keep running (`service`, with `ready`), or use a plugin, a task, or a component; `for_each`, `protect`, `moved:`.
- Tasks with typed inputs as flags, steps, `rescue`/`always`, `retry`, `${{ }}` expressions in CEL, and built-in tasks such as `http` and `kv`.
- `plan`, `up`, and `destroy` in graph order under a lock per unit: a unit runs again only when it changed, after one approval per run.
- Plugins and components from a path or `github.com/opstruss/catalog`, pinned in `opstruss.lock`, and the program contract for plugins.
- Triggers that the daemon of a stack runs: `schedule`, `every`, `watch`, `webhook`, and `after`; `up -d` starts it and `stop` stops it.
- Secrets from sops, the environment, a program, or a plugin, hidden in every output, and sensitive state encrypted with a data key per stack.
- Runs recorded in `.opstruss/` and read by `runs`, `logs`, and `status`; `runs cancel`, `runs rerun`, and `state` repair them.
- `validate`, `explain`, `doctor`, `list`, `outputs`, `exec`, `init`, `uses`, and shell completion; a schema for editors that completes the inputs of what a project uses.
- Output as blocks at a terminal, groups in GitHub Actions, and JSON events with `--json`.
- `tty: true` gives a tool a terminal of its own while the run shows on one, so its color and progress show as they do in a terminal.
- `sudo: true` runs a program as root through sudo, asking for the password at most once a run; plugins and components from git ask for it, and only the files of the project allow it.
- Release builds for Linux and macOS on amd64 and arm64, with checksums, SBOMs, and build provenance, and a script that installs them; documentation at opstruss.github.io.

[Unreleased]: https://github.com/opstruss/opstruss/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/opstruss/opstruss/releases/tag/v1.0.0
