# Pass results between tools

The outputs of a unit are values: text, numbers, lists, and objects. A
unit reads the outputs of another with
`${{ units.<unit>.outputs.<name> }}`, which also makes it run after that
unit. The tool that makes a value and the tool that reads it know nothing
of each other: OpsTruss carries the value between them.

<<< @/examples/outputs/opstruss.yaml

`dns` stands in for a tool such as an OpenTofu module, and prints what it
made as JSON. Each of the other units reads a part of it, in its own way:

```console
$ opstruss up
  + dns    new
  + app    new · after dns (env.HOST, env.ZONE_ID)
  + check  new · after dns (with.ttl)
  + purge  new · after dns (stdin)

Plan of stack default: 4 to run

Apply dns, app, check, purge? [y/N] y
▸ dns › up
│ captured stdout as outputs.zone (105 B, json)
✓ dns › up: done  0.1s
▸ check › check-ttl
│ ttl 300 is a number of seconds
✓ check › check-ttl  0.0s
▸ app › up
│ deploying to www.example.com in zone z-example.com
✓ app › up: done  0.1s
✓ check › up: done  0.1s
▸ purge › up
│ {"api":"api.example.com","www":"www.example.com"}
✓ purge › up: done  0.1s

Up of stack default: 4 done
```

## Where outputs come from

- **A plugin** gives the outputs of its tool. With the OpenTofu plugin,
  each `output` block of the module is an output of the unit, and a
  sensitive one stays sensitive. A unit that reads it does not change when
  `dns` becomes a module:

  ```yaml
  dns:
    uses: ops.tofu
    with:
      dir: ./dns
  ```

  [Your first project](../getting-started/#an-opentofu-module) runs such a
  module.
- **A program** writes to the file `$OPSTRUSS_OUTPUT`: lines
  `name=value`, blocks `name<<EOF` … `EOF` for text of several lines, or
  one JSON object, whose types are kept. Only the names under `outputs:`
  are kept; another name is left out with a warning, such as
  `OPSTRUSS_OUTPUT has extra, which no output declares`. Text takes the
  `type` of its output, so `id=42` with `type: integer` is the number 42.
- **A stream or a file**: `from: stdout`, `stderr`, or `file` (with
  `path`), read in a `format`: `text`, `json`, `yaml`, `lines`, or
  `dotenv`. A stream that becomes an output is not in the log; one line
  says what was captured, as for `dns` above. A file that is not there
  gives `null`.
- **A task**: a unit that uses a task gets the outputs of that task.

The record of the unit keeps its outputs, at most 1 MiB per unit, so a
later run reads them without running the unit again. A large file stays
where it is, and an output holds its path.

## Read them

- A field: `units.dns.outputs.zone.records.www`, or
  `units.dns.outputs.zone['records']` for a key that is not a name.
- An instance of a unit with `for_each`:
  `units.<name>['<key>'].outputs.<output>`.
- A value that is one whole `${{ }}` keeps its type: `ttl` reaches the
  task `check-ttl` as the number 300, which its input checks with
  `type: integer` and `minimum: 60`.
- `${{ }}` inside text gives a string. A number or a boolean becomes
  text; a list or an object stops the unit when it runs:

  ```console
  default/bad: run: item 1: ${{ units.dns.outputs.zone.records }} is object inside text: write the whole value as one ${{ }}, or use toJSON()
  ```

- A value of `env:` and an item of a `run` list are always strings.

[Expressions](../reference/expressions#types) has every rule of types.

## In the format of the next tool

| Where | What the tool gets |
|---|---|
| `with:` | Typed inputs of a plugin or a task, such as `vars` of an OpenTofu module |
| `env:` | A variable, such as `HOST`, which a Compose file reads as `${HOST}` |
| `env: {file: …}` | A variable that holds the path of a file with the value, for a tool that wants a file, such as `KUBECONFIG` |
| `stdin:` | The value on standard input, written in a `format`, as `purge` gets every record as JSON |
| a `write` step | A file in a `format`, such as an inventory or a config, for a tool that reads files |

## Order

- A unit runs after the units it reads. Units that do not read each other
  run at once, up to `OPSTRUSS_PARALLEL` (10 by default).
- `needs:` orders units without data: `purge` runs after `app`, which it
  does not read. A `needs` on a unit that `if` leaves out of the stack is
  met.
- `validate` refuses a cycle.
- In a task, `steps` run in order, and `needs:` runs other tasks first.

## What plan and up show

`plan after default/dns (env.HOST, env.ZONE_ID)` says that the unit reads
`dns`, which changes first, and which parts of the unit read it. Such a
unit is planned in its turn, with the new values, never with the old ones.

A unit runs again only when what it reads changes. With another zone,
`dns` changes its records, so `app` and `purge` run again, and `check`
does not, since `ttl` is still 300:

```console
$ opstruss up --input zone=example.org
  + dns    definition changed: env.ZONE
  ? app    after dns (env.HOST, env.ZONE_ID)
  ? check  after dns (with.ttl)
  ? purge  after dns (stdin)

Plan of stack default: 1 to run, 3 may run

Apply dns, app, check, purge? [y/N] y
▸ dns › up
│ captured stdout as outputs.zone (105 B, json)
✓ dns › up: done  0.1s
✓ check › up: unchanged
▸ app › up
│ deploying to www.example.org in zone z-example.org
✓ app › up: done  0.1s
▸ purge › up
│ {"api":"api.example.org","www":"www.example.org"}
✓ purge › up: done  0.1s

Up of stack default: 3 done, 1 unchanged
```

## See the outputs

| Command | Shows |
|---|---|
| `opstruss status dns` | The stored outputs of the unit, with sensitive values hidden |
| `opstruss list --json` | Each unit with its outputs and whether each one is sensitive |
| `opstruss outputs` | The `outputs:` of the project, for people and for other projects |

`outputs:` of the project names what others read, here `www`:

```console
$ opstruss outputs
www: www.example.org
```

## Limits

- The outputs of a task or a step live for one run; they do not become
  outputs of a unit.
- `if` and `for_each` of a unit read only outputs that are known when the
  run starts. One that reads a unit that changes first in the same run
  cannot be planned:

  ```console
  default/per: error: if and for_each of per are known when the run starts, but this run changes dns first; run opstruss up dns, then this
  ```

- A value that must be fresh in every run, such as a short-lived token,
  comes from a unit that runs in every run: `revision: ${{ opstruss.run }}`
  with `approval: auto`.
- Another project is read by a unit that runs its `outputs`:

  ```yaml
  network:
    run: [opstruss, -C, ../network, outputs, --stack, prod, --json]
    outputs:
      net: {from: stdout, format: json}
  ```

  Units then read `${{ units.network.outputs.net.outputs.<name>.value }}`.
