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
  cat: github.com/opstruss/catalog@v1
  tofu:
    source: cat.tofu
    pass: [AWS_PROFILE, AWS_REGION]
```

`opstruss.lock` pins the commit of `v1` for every plugin of the catalog,
and `opstruss plugins update cat` moves them all at once. Commit the lock
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

Each `opstruss.yaml`, also of a component, starts with `opstruss: "1.0"`,
the version it is written for, so a newer OpsTruss reads it as this one
did, and an older one says that it is too old.
