# Compatibility

OpsTruss follows [Semantic Versioning](https://semver.org). Within a
major version, such as 1.x, a minor or patch release keeps every project,
component, plugin, and script that reads `--json` working as it did,
without a change on their side.

## What stays

| Area | Promised |
|---|---|
| Files | The keys of `opstruss.yaml`, `opstruss.plugin.yaml`, `opstruss.catalog.yaml`, and `opstruss.lock` and what they mean; built-in tasks; the keywords and functions of expressions |
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
- **Data has versions.** Everything OpsTruss writes has a version, which
  goes up only when an older OpsTruss would read the data wrong. An older
  OpsTruss refuses a newer version, and says to upgrade; a newer one reads
  every older version and migrates it when it writes. `opstruss.lock`,
  which a team shares, keeps its version unless what it holds needs a
  newer one.
- **Schemas stay where they are.** The schema of each minor version stays
  at `https://opstruss.github.io/v<major>.<minor>/schema/`, which
  `opstruss init` names on the first line of `opstruss.yaml`; a patch
  release never makes a file that was valid there invalid. A project that
  raises its `opstruss:` points that line at the schema of the new level.

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
