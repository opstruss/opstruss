---
layout: home
hero:
  name: OpsTruss
  text: Event-driven workflow orchestration
  tagline: Runs any tool and hands its typed results to the next.
  actions:
    - theme: brand
      text: Get started
      link: /getting-started/
    - theme: alt
      text: Install
      link: /getting-started/install
features:
  - title: Plan, then apply
    details: up shows what would change and asks once before it changes anything; in CI, --yes approves and exit code 2 means a change waits for approval.
  - title: Only what changed runs
    details: A unit runs again when its program, inputs, or source files change, in the order of what it reads, as make and Taskfile skip what is up to date.
  - title: Typed results between tools
    details: The outputs of an OpenTofu module, a Compose project, or any program reach the next unit as numbers, lists, and objects, never as text to parse.
  - title: Triggers and services
    details: The daemon of a stack keeps services running and runs tasks on a schedule, a webhook, or a change of files, with secrets hidden in every log.
---

## One file for the whole project

```yaml
# opstruss.yaml
opstruss: "0.1"
name: shop
uses:
  cat: github.com/opstruss/catalog@v1
units:
  infra:
    uses: cat.tofu
    with:
      dir: ./infra
  web:
    uses: cat.compose
    env:
      DATABASE_URL: ${{ units.infra.outputs.database_url }}
tasks:
  backup:
    on:
      schedule:
        cron: "0 3 * * *"
    run: ./backup.sh
```

`opstruss up` applies the OpenTofu module, then starts the Compose project
with what the module made; `opstruss up -d` also runs `backup` every
night. [Your first project](./getting-started/) builds one in fifteen
minutes.
