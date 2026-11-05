# Compatibility

OpsTruss follows [Semantic Versioning](https://semver.org). Within a
major version, such as 1.x, a minor or patch release keeps every project,
component, plugin, and script that reads `--json` working as it did,
without a change on their side.

## What stays

| Area | Promised |
|---|---|
| Files | The keys of `opstruss.yaml`, `opstruss.plugin.yaml`, `opstruss.catalog.yaml`, and `opstruss.lock`, what they mean, and their defaults; built-in tasks; the keywords and functions of expressions |
| Command line | Commands, built-in flags, exit codes, the fields of `--json` that [Exit codes and JSON](./exit-codes-and-json) lists, and the [variables](./environment) that it reads |
| Plugins | The [program contract](./program-contract): what a program gets and gives |
| State | Records, the `_meta` of each stack, `kv`, the file of each daemon, the shape of events, addresses, run ids, and the values of `outcome` |
| Webhooks | The status, `Retry-After`, and JSON body of each [answer of the daemon](../guides/triggers#the-daemon) |
| Releases | The names of the files of a release, `checksums.txt`, and what each archive holds: `opstruss` at its root, with `LICENSE`, `README.md`, and `CHANGELOG.md` |

## What may change

- What OpsTruss prints for people: the text at a terminal, colors, the
  layout of lines, and error messages. Scripts read `--json` instead.
- Behavior that these pages do not describe, and fields of `--json` that
  they do not list.
- Files that are not valid: the messages, where they point, their order,
  and which problem comes first. A file that a version refuses may become
  valid in a later one.
- Bugs: where OpsTruss does not do what these pages say, a later release
  makes it do so.
- Where files lie under `.opstruss/runs/`, `logs/`, and `work/`, and the
  cache in `~/.opstruss/`.
- The API between the command line and the daemon, which both come from
  one binary.
- What `OPSTRUSS_LOG=debug` prints.
- What a version does not support yet, such as backend plugins and
  `mcp:`, which it refuses; a later version may support them.
- Builds that are not releases: a build of `main` or of any other
  commit, and the docs under `/dev/`.
- Going back: an upgrade from any 1.x to a later 1.x keeps working; going
  back to an earlier one is not promised, since an older version refuses
  data and levels that are newer than it.
- The order of the events and lines of units that run at once, how long
  a run takes, and the memory and disk it uses; the [limits](#limits)
  only grow.
- Systems outside OpsTruss: GitHub, the sources in `uses:`, and the tools
  that plugins run. Plugins and components have versions of their own,
  as [Plugins](../plugins) says.
- Experiments: a feature that the docs call experimental runs only when
  you turn it on, and may change or go away in any release.
- A fix of a security problem, when nothing else fixes it, also one that
  comes from Go or a library.

## Platforms and tools

- Releases are for Linux and macOS on amd64 and arm64. A later version
  may add a platform, and drops one only in a new major version. The
  oldest version of each operating system follows the Go release that
  builds OpsTruss, as the upgrade notes of the
  [changelog](https://github.com/opstruss/opstruss/blob/main/CHANGELOG.md)
  say.
- The oldest `sops` that OpsTruss works with, 3.10.0, and the oldest
  `git` may rise in a minor release, never in a patch, as the upgrade
  notes say; `opstruss doctor` checks `sops`. A new feature may need a
  later version.

## How OpsTruss changes

- **It adds.** A new key, field, variable, or command is optional, and
  without it OpsTruss behaves as before. Readers of events, plugins, and
  OpsTruss itself ignore fields and kinds of events they do not know.
  `outcome`, the `status` of a run, `level`, and `stream` keep their
  values, and details come in new fields; any other list of values may
  grow, as [Exit codes and JSON](./exit-codes-and-json#json) says.
- **Your names win.** A built-in task, keyword, flag, or command that a
  later version adds gives way to a name of yours: a name in `uses:`, a
  task, or an input of the same name keeps working as before, and
  `validate` warns.
- **It starts strict.** Validation and limits start narrow and only grow
  looser, since what a version accepts, a later one accepts too.
- **The level in `opstruss:`.** Each file names the version of OpsTruss it
  is written for: `opstruss.yaml`, `opstruss.plugin.yaml`, and
  `opstruss.catalog.yaml`. An older OpsTruss refuses it before it reads the
  rest, so what a newer version added fails as the version it needs, and a
  newer one keeps the behavior of that level for a change that cannot be
  an addition. The
  [changelog](https://github.com/opstruss/opstruss/blob/main/CHANGELOG.md)
  names each such change.
- **Deprecated is not removed.** What a version deprecates keeps working
  until the next major version, and `validate` and each run warn about
  it. A later level may refuse it in a file that names that level; a file
  that names an older level keeps working as before, for the whole major
  version.
- **Data has versions.** Everything OpsTruss writes has a version, which
  goes up only when an older OpsTruss would read the data wrong. An older
  OpsTruss refuses a newer version, and says to upgrade; a newer one reads
  every older version and migrates it when it writes. `opstruss.lock`,
  which a team shares, keeps its version unless what it holds needs a
  newer one.
- **Schemas stay where they are.** The schema of each minor version stays
  at `https://opstruss.github.io/v<major>.<minor>/schema/`; a patch
  release never makes a file that was valid there invalid. The first line
  that `opstruss init` writes in `opstruss.yaml` names
  `.opstruss/schema.json`, which stays at that path, and which `validate`,
  `uses fetch`, and `uses update` write again; what it holds may change,
  since it only helps an editor and `validate` decides.
- **Links stay.** The page that the help of each command ends with, at
  `https://opstruss.github.io/v<major>.<minor>/reference/cli/<command>`,
  stays for as long as the site keeps that version; a page that moves
  leaves one behind that links to its new place.
- **A break is a bug.** A release that breaks what this page promises
  gets a patch that brings the old behavior back, unless the change fixes
  a security problem; a new feature that conflicts with old behavior
  comes through the level in `opstruss:` or a choice you make. Fixes come
  to the latest minor version, since an upgrade within 1.x is safe.

A change that breaks what this page promises comes only in a new major
version, with its upgrade in the changelog under "Upgrade notes".

## Limits

Each limit only grows in a later version.

| Limit | Value |
|---|---|
| Outputs of a program, from a file or a stream | 1 MiB |
| An input in a variable of the environment; a larger one is only in the file of `OPSTRUSS_INPUTS` | 64 KiB |
| `OPSTRUSS_RESULT` | 64 KiB |
| The output of programs that a run stores; the middle of more is dropped | 10 MiB |
| A value of `kv`, and keys per stack | 1 MiB, 1000 |
| The body of a webhook request | 1 MiB |
| An event of `--event` | 4 MiB |
| What `file()` reads | 1 MiB |
| Runs that a daemon keeps waiting or running per task | 100 |
| Finished runs kept per task and stack, and the size of `runs/` per stack | 100, 1 GiB |
