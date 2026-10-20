# Changelog

Every change to OpsTruss that an operator, or the author of a component or
a plugin, can notice is written down here, newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
with one addition: a release that asks something of a project, a component,
or a plugin says so first, under "Upgrade notes". OpsTruss follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). Until 1.0.0 a
minor release can change what a project, a component, or a plugin relies on.

## [Unreleased]

### Added

- `opstruss validate` checks `opstruss.yaml`, its include files, and every
  stack against the schema and the rules of the model, among them that
  every `${{ }}` compiles and reads only what exists and may be read where
  it is written, and that units, task calls, and locals form no cycle. It
  names the file, line, and column of each problem; `--json` prints one
  document.
- `opstruss explain` prints the project of one stack as read: include files
  and stack overrides merged, each project input with where its value came
  from (`--input`, the stack, `env`, or `default`), the value of each local,
  and the order of the units; names pick units and tasks with what they
  depend on.
- `${{ }}` is CEL with the cel-go extensions for strings, lists, math, sets,
  two-variable comprehensions, bindings, encoders, and optional values, plus
  `toJSON()`, `fromJSON()`, `toYAML()`, `fromYAML()`, `sha256()`, `file()`,
  and `strftime()` on timestamps. A value that is one `${{ }}` keeps its
  type, and maps are iterated in key order.
- `opstruss state list`, `pull`, `push`, and `rm` read and repair the
  records of a stack under their locks: `push` takes the etag of a `pull`,
  or `--force`, and refuses a stale copy, a record for another address,
  and a record of a newer OpsTruss; `rm` forgets a record without taking
  anything down and leaves a tombstone, and removes a tombstone.
- State lives in `.opstruss/state/`, one record per address, written
  atomically with a backup of the previous content. `_meta` names the
  project a stack belongs to, and a stack of another project, of a newer
  layout, or with records but no `_meta` is never written.
- `-C <dir>`, `--stack` (or `OPSTRUSS_STACK`), `--input name=value`, and
  `--json`.
- `opstruss --completion bash|zsh|fish` prints a completion script.
- `opstruss version` prints the version and the platform.
- Release builds for Linux and macOS on amd64 and arm64, with checksums,
  SBOMs, and build provenance attestations.

[Unreleased]: https://github.com/opstruss/opstruss/commits/main
