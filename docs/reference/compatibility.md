# Compatibility

OpsTruss follows [Semantic Versioning](https://semver.org). Within a
major version, such as 1.x, a minor or patch release keeps every project,
component, plugin, and script that reads `--json` working as it did,
without a change on their side.

## What stays

| Area | Promised |
|---|---|
| Files | The keys of `opstruss.yaml`, `opstruss.plugin.yaml`, and `opstruss.lock` and what they mean; built-in tasks; the keywords and functions of expressions |
| Command line | Commands, built-in flags, exit codes, and the JSON of `--json`: events and documents |
| Plugins | The [program contract](./program-contract): what a program gets and gives |
| State | Records, the `_meta` of each stack, `kv`, the file of each daemon, the shape of events, addresses, run ids, and the values of `outcome` |

## What may change

- What OpsTruss prints for people: the text at a terminal, colors, the
  layout of lines, and error messages. Scripts read `--json` instead.
- Where files lie under `.opstruss/runs/`, `logs/`, and `work/`, and the
  cache in `~/.opstruss/`.
- The API between the command line and the daemon, which both come from
  one binary.
- What `OPSTRUSS_LOG=debug` prints.
- What a version does not support yet, such as backend plugins and
  `mcp:`, which it refuses; a later version may support them.
- A fix of a security problem, when nothing else fixes it.

## How OpsTruss changes

- **It adds.** A new key, field, variable, or command is optional, and
  without it OpsTruss behaves as before. Readers of events, plugins, and
  OpsTruss itself ignore fields and kinds of events they do not know.
  `outcome` keeps its four values; details come in new fields.
- **It starts strict.** Validation and limits start narrow and only grow
  looser, since what a version accepts, a later one accepts too.
- **The level in `opstruss:`.** Each file names the version of OpsTruss it
  is written for. An older OpsTruss refuses it, and a newer one keeps the
  behavior of that level for a change that cannot be an addition. The
  [changelog](https://github.com/opstruss/opstruss/blob/main/CHANGELOG.md)
  names each such change.
- **Data has versions.** Everything OpsTruss writes has a version. An older
  OpsTruss refuses data that a newer one wrote, and says to upgrade; a
  newer one reads every older version and migrates it when it writes.

A change that breaks what this page promises comes only in a new major
version, with its upgrade in the changelog under "Upgrade notes".
