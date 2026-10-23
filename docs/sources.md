# Source dependencies

Declare a prepared local directory or an official remote source:

```yaml
uses:
  demo: ../plugins/demo
  remote: github.com/opstruss/plugins/demo@v1
```

Local paths use the operator's working tree, including uncommitted changes.
Local components are trusted. Plugin environment variables remain requests
that the project must grant with `pass:`. Compiled plugins do not build
implicitly: build locally and point `run:` at the executable for development.

Automatic remote fetching is restricted to `github.com/opstruss/plugins`.
Other repositories can be prepared as local directories. `main`, `dev`, and
`latest` are ordinary Git refs; `latest` must exist as a branch or tag.

## Pins and commands

`opstruss.lock` contains version 1 source-to-commit/hash pins, including
transitive components and plugins. Commit this file with the project. Every
subdirectory using the same repository and ref shares one commit. Normal
execution adds missing pins for the selected stack, preserving existing
pins. `--locked` refuses any pin change, while permitting downloads of
already pinned commits.

- `opstruss plugins` lists declarations, known transitive pins, and cache
  status without fetching. `--json` preserves full commits and hashes.
- `opstruss plugins fetch` prepares all stacks and the current platform's
  binaries. It preserves pins and explicitly repairs corrupt cache entries
  from their original commit/checksum. `--locked` requires complete pins.
- `opstruss plugins update [name]` explicitly resolves refs again. An
  optional name selects a root `uses:` declaration and its repository/ref
  group; all affected pins are reported. A full update also removes pins
  no longer reached by any stack.
- `opstruss plugins prune` removes shared cache entries unused for 30 days,
  skipping active runs, services, and trigger snapshots. It also collects
  abandoned temporary downloads and never removes project state or local
  checkouts.

Fetch and update cover the whole project and reject explicit `--stack`.
Help/completion use only available metadata. Fetch first to discover tasks
of remote components whose metadata is missing; root tasks can prepare
sources when executed.

## Integrity and permissions

Snapshots are read-only and verified against the complete Git tree before
use, including executable modes, relative symlinks, and Gitlink commits.
Relative source and include paths stay inside the repository root. Git
submodules are not recursively fetched. The hash includes every file in
the repository, even outside the selected plugin directory.

Remote components cannot grant input `env:`, `approval: auto`, or `yes: true`.
Their `pass:` requests can only narrow their caller's grants. Programs run
as the operator; the source checks do not constitute a process sandbox.

The cache is `~/.opstruss/cache/`. A complete warm cache works without Git
or network access. Missing sources fail before execution, and corrupt
entries require explicit fetch to repair; a run never accepts a new hash
for a corrupt pinned entry.

## Binary manifests

A plugin can declare executable names and all supported platform assets:

```yaml
bin:
  demo-tool:
    linux/amd64:
      url: https://github.com/opstruss/plugins/releases/download/v1/demo-tool.tar.gz
      sha256: <64 lowercase hexadecimal characters>
```

Platforms are `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`;
the current platform must be declared. Automatic URLs are official HTTPS
release assets. `GITHUB_TOKEN` is optional for the download request and is
not forwarded to another origin on redirect.

Payloads can be direct executables or `.tar.gz` archives. An archive must
contain an executable regular file at its root named exactly as the `bin`
key; other regular files are ignored. Absolute/traversal paths, duplicate entries, links,
special files, and unsafe modes are rejected. Downloads/extraction are
limited to 512 MiB and archives to 4096 entries. Both cached payloads and
executables are checked. The executable directory precedes PATH only for
programs of the declaring plugin, including hooks and `requires`/`doctor`.
A missing platform or failed checksum never falls back to an operator tool.

## Cleanup after updates

Updating pins does not change existing resources. Each resource and
unfinished attempt keeps its own resolved cleanup contract and immutable
source identity. Cleanup restores those commits and binary checksums
without consulting the latest ref or re-evaluating current definitions.
Offline cleanup succeeds with complete cached dependencies; missing or
corrupt old dependencies leave the record intact.

Restoration uses the original canonical cache layout. Explicit source
locations are stored relative to their snapshot. Absolute paths embedded
in scripts, argv, and environment remain machine context; moving the cache
root or relocating that context is not supported. An unsafe restoration
fails before executing cleanup. Local cleanup still needs its working tree.
