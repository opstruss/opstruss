# Your first project

In about fifteen minutes, this page builds a small project: a unit that
builds a page, an OpenTofu module, a unit that reads what both give, a
task, and two stacks. Along the way it shows `plan`, `up`, and `destroy`.

You need `opstruss` ([Install](./install)) and `git`, and in `PATH` two
programs that the OpenTofu plugin runs: `tofu`, the
[OpenTofu](https://opentofu.org) command, and `jq`, for example with
`mise use opentofu jq`.

## Start a project

```sh
mkdir shop && cd shop
opstruss init
```

```console
wrote opstruss.yaml
wrote .gitignore
next: add units or tasks to opstruss.yaml, then run opstruss plan
```

`opstruss.yaml` names the project and the version of OpsTruss it is
written for. Its first line names the schema of the file, so an editor
with the YAML language server completes and checks it
([Editor support](./install#editor-support)):

```yaml
# yaml-language-server: $schema=https://opstruss.github.io/v1.0/schema/project.json
opstruss: "1.0"
name: shop
```

`.gitignore` leaves out `.opstruss/`, where OpsTruss keeps the state of
the project on this machine.

## A unit that builds a page

A unit is something that `up` brings up and `destroy` takes away. The
simplest one runs a program. Write a page:

```sh
mkdir site
echo '<h1>shop</h1>' > site/index.html
```

and add the unit under a new key `units:` of `opstruss.yaml`:

<<< @/examples/first-project/opstruss.yaml#site

::: details opstruss.yaml so far
```yaml
# yaml-language-server: $schema=https://opstruss.github.io/v1.0/schema/project.json
opstruss: "1.0"
name: shop
units:
  site:
    run: |
      mkdir -p dist
      cp site/index.html dist/index.html
      echo "page=dist/index.html" >> "$OPSTRUSS_OUTPUT"
    sources: [site/**]
    generates: [dist/index.html]
    outputs:
      page: {}
```
:::

- `run` is the program. A string is a script for `sh`; a list, such as
  `[echo, hi]`, runs as written, without a shell.
- A line `name=value` in the file `$OPSTRUSS_OUTPUT` is an output of the
  unit, here `page`, which other units read.
- `sources` are the files whose content is part of the unit, and
  `generates` the files it makes: the unit runs again when a source
  changes or a generated file is missing.

`plan` shows what `up` would do, without changing anything:

```console
$ opstruss plan
  + site  new

Plan of stack default: 1 to run
```

`up` asks before it changes anything, then runs the unit:

```console
$ opstruss up
  + site  new

Plan of stack default: 1 to run

Apply site? [y/N] y
✓ site › up: done  0.1s

Up of stack default: 1 done
```

Run `opstruss up` again: nothing changed, so nothing runs. Change
`site/index.html`, and the plan says why the unit runs again:

```console
$ opstruss plan
  + site  definition changed: sources

Plan of stack default: 1 to run
```

## An OpenTofu module

Plugins wrap tools as units. The [catalog](../plugins) has a plugin for
OpenTofu; declare the catalog once under `uses:`, and the unit uses its
`tofu` plugin as `cat.tofu`:

```yaml
uses:
  cat: github.com/opstruss/catalog@v1
```

Write a module in `infra/main.tf`, after `mkdir infra`:

<<< @/examples/first-project/infra/main.tf

and the unit that applies it, under `units:` next to `site`:

<<< @/examples/first-project/opstruss.yaml#infra

`with:` gives the inputs of the plugin: the directory of the module, and
the values of its variables. `${{ stack }}` is the name of the stack,
`default` until the project has stacks of its own. The outputs of the
module, here `url`, become the outputs of the unit.

## A unit that reads both

Add a unit that reads the outputs of the other two:

<<< @/examples/first-project/opstruss.yaml#publish

::: details opstruss.yaml so far
```yaml
# yaml-language-server: $schema=https://opstruss.github.io/v1.0/schema/project.json
opstruss: "1.0"
name: shop
uses:
  cat: github.com/opstruss/catalog@v1
units:
  site:
    run: |
      mkdir -p dist
      cp site/index.html dist/index.html
      echo "page=dist/index.html" >> "$OPSTRUSS_OUTPUT"
    sources: [site/**]
    generates: [dist/index.html]
    outputs:
      page: {}
  infra:
    uses: cat.tofu
    with:
      dir: ./infra
      vars:
        name: shop-${{ stack }}
  publish:
    run: [echo, "publishing ${{ units.site.outputs.page }} to ${{ units.infra.outputs.url }}"]
```
:::

`${{ units.site.outputs.page }}` makes `publish` come after `site`, and
`${{ units.infra.outputs.url }}` after `infra`, so `up` runs them in that
order, and the plan of the module shows what it will create:

```console
$ opstruss up
▸ infra › plan
│ OpenTofu will perform the following actions:
│
│   # terraform_data.site will be created
│   …
✓ infra › plan  1.3s

  + infra    new · create terraform_data.site, output.url
  + site     definition changed: sources
  + publish  new · after infra, site (run)

Plan of stack default: 3 to run

Apply infra, site, publish? [y/N] y
✓ site › up: done  0.1s
▸ infra › up
│ terraform_data.site: Creating...
│ …
✓ infra › up: done  0.3s
▸ publish › up
│ publishing dist/index.html to https://shop-default.example.com
✓ publish › up: done  0.1s

Up of stack default: 3 done
```

The first command that read `uses:` fetched the catalog and wrote the
commit that `v1` names to `opstruss.lock`. Commit that file with the
project, so that every machine and CI run the same plugins until
`opstruss plugins update` moves the pin. The first `up` of `infra` also
wrote `infra/.terraform.lock.hcl`, the providers that OpenTofu chose,
which is committed too.

## A task

A task is a command of the project, with its inputs as flags:

<<< @/examples/first-project/opstruss.yaml#tasks

```console
$ opstruss smoke --path /health
▸ smoke
│ GET https://shop-default.example.com/health
✓ smoke  0.0s
```

`opstruss --help` lists it under "Project tasks", and `opstruss list`
shows each task with its last run, and each unit with its state.

## Two stacks

A stack is a copy of the units with a state of its own, such as `dev`
and `prod`. Take the stack `default` down first, since a project with
stacks of its own has no `default`:

```console
$ opstruss destroy
  - infra    destroy
  - publish  forget: it has no destroy program
  - site     forget: it has no destroy program

Plan of stack default: 1 to destroy, 2 to forget

Destroy infra, forget publish, site? [y/N] y
✓ publish › destroy: forgotten without a destroy program  0.0s
✓ site › destroy: forgotten without a destroy program  0.0s
▸ infra › destroy
✓ infra › destroy: done  0.7s

Destroy of stack default: 3 done
```

A unit without a `destroy` program has nothing to take away, so its
record is forgotten; `infra` runs `tofu destroy`. A stack that the
project no longer declares keeps its records, and `plan` warns about
them until `opstruss destroy --stack default` takes them down. Now
declare the stacks:

```yaml
stacks:
  dev: {}
  prod: {}
```

With more than one stack, each command names one:

```sh
opstruss up --stack dev
opstruss up --stack prod
opstruss smoke --stack prod
```

`shop-dev` and `shop-prod` are two resources, each with its own state.
To leave out `--stack` in a directory, set `OPSTRUSS_STACK`, for example
in `mise.toml`:

```toml
[env]
OPSTRUSS_STACK = "dev"
```

Without mise, `export OPSTRUSS_STACK=dev` in an `.envrc` of direnv, or in
the shell, does the same.

The whole project, which the tests of OpsTruss run, is in
[`docs/examples/first-project`](https://github.com/opstruss/opstruss/tree/main/docs/examples/first-project).

## Back up the state

OpsTruss keeps the state of each stack in `.opstruss/state/` of the
project: the records of the units, the `_meta` of each stack, and the
state of tools such as `terraform.tfstate`, in plain text. Sensitive
values in it are encrypted with the age key of this machine, which
`opstruss doctor` shows. On one machine, these are the only copies: back
up `.opstruss/state/` and the key, and remember that `.gitignore` leaves
`.opstruss/` out of git, so `git clean -x` deletes it.

## Clean up

```sh
opstruss destroy --stack dev
opstruss destroy --stack prod
```

## Next

- [Structure a project](../guides/structure-a-project): components,
  versions of tools, and the catalog.
- [Secrets with sops](../guides/secrets): credentials for the tools.
- [`opstruss.yaml`](../reference/opstruss-yaml): every key.
