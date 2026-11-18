# Structure a project

A project lasts longer than its tools: a database that runs in Docker
Compose today may run from an OpenTofu module tomorrow. This page keeps
the files stable while the tools change.

```text
mise.toml                    versions of the tools; OPSTRUSS_STACK per directory
opstruss.yaml                what connects: uses, inputs, stacks, secrets, units, tasks
opstruss.lock                committed; CI checks it with --locked
components/<capability>/     db, dns, site: each with its own inputs and outputs
infra/ compose.yaml …        the files of the tools, as the tools read them
secrets/<stack>.env, .sops.yaml
```

## One component per capability

A component is a project used as a unit. Write one per capability, not
per tool, and let other units read only its outputs:

<<< @/examples/structure/opstruss.yaml

<<< @/examples/structure/components/db/opstruss.yaml

`app` reads `units.db.outputs.host`, never the units inside `db`, so
`db` can move from one tool to another inside the component without a
change to `app`. When an address
changes, `moved:` moves its record instead of destroying it:

```yaml
moved:
  - {from: database, to: db}
```

## Stacks differ by their values

What differs between `dev` and `prod` is in `stacks.<name>.with` and in
the secret file of each stack, as `db_size` above, rather than in `if:`
spread over the units. Each stack has a state of its own, and commands
name it with `--stack`, or with `OPSTRUSS_STACK`.

## Global units and tasks

Some units are one for the whole project, whatever the stack: a bucket
for state, a shared DNS zone, a base image, the tools of a machine. Write
them with `global: true`:

```yaml
opstruss: "1.1"
units:
  tools:
    global: true
    uses: ops.mise
  zone:
    needs: [tools]
    uses: tofu
    with:
      source: ./zone
```

A global unit has one record, in `.opstruss/state/_global/`, and its
address is `_global/tools`. `opstruss up --stack dev` also brings up the
global units that the units of `dev` need, in the same plan and the same
question, and `opstruss up --stack prod` then finds them unchanged; a
global unit that no unit needs runs only when named. A change to a
global unit asks for approval even when the stack has `approval: auto`,
since it reaches every stack. `destroy --stack dev` leaves the global
units up.

| Command | Runs |
|---|---|
| `opstruss up tools` | the global unit `tools`, without `--stack` |
| `opstruss up --global` | every global unit; also `plan`, `destroy`, `status`, `logs`, `outputs`, `runs`, and `state` |
| `opstruss up --stack dev` | the units of `dev`, and the global units they need |
| `opstruss <task>` | a task with `global: true`, without `--stack` |

What a global unit or task reads is the same for every stack: no
`stack`, no unit of a stack, no input that `stacks.<name>.with` gives,
and no local or secret that reads one of those. `validate` says where,
such as `global unit tools reads inputs.region, which stacks dev, prod
give through with:`. A global task runs only by hand or after other runs
(`after:`), since the daemon runs per stack, and a global unit is never a
service.

A unit that becomes global leaves the records of its stacks behind as
orphans, which `plan` names. Take each one out without destroying it,
then let the global unit take its place:

```sh
opstruss state rm --stack dev tools
opstruss state rm --stack prod tools
opstruss up tools
```

## Versions in mise.toml

The versions of OpsTruss and of the tools are in `mise.toml`, and
`requires:` names only the programs:

```toml
[tools]
"github:opstruss/opstruss" = "1.0.0"
opentofu = "1.13.2"
jq = "1.8.2"

[env]
OPSTRUSS_STACK = "dev"
```

`opstruss doctor` shows where each program is found. Without mise, any
package manager can install them; OpsTruss only checks that they are in
`PATH`.

## One pin of the catalog

Declare the catalog once, and name its plugins through it:

```yaml
uses:
  ops: github.com/opstruss/catalog@v1
  tofu:
    source: ops.tofu
    pass: [AWS_PROFILE, AWS_REGION]
```

`opstruss.lock` pins the commit of `v1` for every plugin of the catalog,
and `opstruss uses update ops` moves them all at once. Commit the lock
file; CI runs with `--locked`, which fails rather than changing it.

## Values reach programs through env

A value of an expression goes to a program through `env:` or an item of
a `run` list, never inside a script, so a value with quotes or `$` stays
as it is:

```yaml
run: echo "deploying $IMAGE"
env:
  IMAGE: ${{ units.build.outputs.image }}
```

## Every file says its version

Each `opstruss.yaml`, also of a component, starts with `opstruss:`, the
version it is written for, such as `"1.0"`, so a newer OpsTruss reads it
as this one did, and an older one says that it is too old. A key that a
later version added needs that version there: `global:` needs
`opstruss: "1.1"`, and a file that says `"1.0"` gets `global needs
opstruss: "1.1" or later in this file, which says 1.0`.
