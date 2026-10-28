# Expressions

A value in `${{ }}` is an expression in [CEL](https://cel.dev), the
Common Expression Language. It reads inputs, the outputs of units, tasks, and steps,
secrets, and facts of the run.

```yaml
units:
  web:
    uses: cat.compose
    env:
      IMAGE: ${{ units.build.outputs.image }}
```

## What an expression reads

| Name | What it is |
|---|---|
| `inputs.<name>` | An input of the task, the component, or the project, through its stack |
| `locals.<name>` | A local of the project or the component |
| `units.<name>.outputs.<key>` | A stored output of a unit |
| `units.<name>['<key>'].outputs.<k>` | One instance of a unit with `for_each` |
| `units['my-db'].outputs.<key>` | A unit whose name has a `-` |
| `units.<name>.changed`, `.outcome`, `.plan` | Whether the unit changed something in this run, how it ended (`success`, `failure`, `skipped`, `cancelled`), and its last plan: `changes`, `outputs`, `exact`, `digest`, `summary` |
| `tasks.<name>.outputs.<key>` | An output of a task in the same run |
| `steps.<id>.outputs.<key>` | An output of a step of the same task |
| `steps.<id>.changed`, `.outcome` | Whether the step changed something, and how it ended |
| `steps.<id>.units['<address>']` | What a `task: up` step, or another task of units, did to each instance |
| `secrets.<name>` | A secret, hidden in what OpsTruss writes |
| `stack` | The name of the stack |
| `each.key`, `each.value` | The item of `for_each` |
| `event.<field>` | The event of a trigger; `event.time` and `event.trigger` are in every run |
| `outputs` | In `retry.until`, `failed_when`, and `changed_when`: the outputs being judged |
| `opstruss.project` | The `name` of the root project, also inside a component |
| `opstruss.address`, `opstruss.dir` | The address of the unit, and the directory of the file that writes it |
| `opstruss.os`, `.arch`, `.host`, `.time`, `.run` | Facts of the machine and the run; `time` is when the run started |
| `opstruss.cwd` | Where OpsTruss started, before `-C`; in tasks and steps only |
| `opstruss.artifacts`, `opstruss.temp` | `$OPSTRUSS_ARTIFACTS` and `$OPSTRUSS_TEMP` of the run; in tasks and steps only |
| `opstruss.state_key`, `opstruss.reason` | In a task that a unit uses: `$OPSTRUSS_STATE_KEY` and `$OPSTRUSS_REASON` |

There is no `env` namespace: a value from the environment comes in as an
input with `env:`, or a secret with `env:`.

A unit, task, or step with `for_each` is a map from key to the result of
each instance. A macro
combines them, such as
`steps.attempts.exists(k, v, v.outcome == 'success')`.

## Functions

OpsTruss adds:

| Function | What it gives |
|---|---|
| `file('<path>')` | The text of a file, relative to the file that writes the expression; at most 1 MiB |
| `kv('<key>')`, `kv('<key>', <default>)` | A value of the [memory of the stack](./builtin-tasks#kv), or `null` or the default; `kv('<prefix>*')` gives every key with the prefix as a map |
| `toJSON()`, `fromJSON()`, `toYAML()`, `fromYAML()` | Data as text, and text as data |
| `sha256()` | The SHA-256 of a string, in hex |
| `<timestamp>.strftime('<format>', '<timezone>')` | A time as `strftime` writes it, in UTC without a time zone |

Yesterday in Jakarta:
`(opstruss.time - duration('24h')).strftime('%Y-%m-%d', 'Asia/Jakarta')`.

Standard CEL has `size`, `contains`, `startsWith`, `endsWith`, `matches`,
`int`, `string`, `has`, `map`, `filter`, `all`, `exists`, `timestamp()`,
`duration()`, and the getters of time. The official extensions of cel-go
are there too: strings (`join`, `split`, `trim`, `replace`, `format`, …),
lists (`lists.range`, `slice`, `flatten`, `distinct`, `sort`, …), math,
sets, two-variable comprehensions (`transformList`, `transformMap`, …),
optional values (`?.`, `orValue`), `cel.bind`, and `base64`.

## Types

- A value that is a single `${{ }}` keeps the type of its result: a list,
  a map, a number, or a boolean, also in quotes in YAML.
- `${{ }}` inside text gives a string. Numbers and booleans become text; a
  list, a map, or `null` is refused, with `toJSON()` as the way to write
  it.
- A value of `env:` and an item of a `run` list are always strings.
- A number takes the type of the input or output it goes to: a `number` is
  a `double`, also when it is whole, and an `integer` is an `int`, which
  holds 64 bits, so an id larger than that is a string. Without a type, as
  from `fromJSON()`, a whole number becomes an `int` and any other a
  `double`, so `80` and `80.0` from two tools are equal. An expression
  does not mix the two, as in `2 / 4.0`; write `double(x)` for a value
  without a type.
- A map is always read in the order of its keys, in macros, in `for_each`,
  and as a list, so the same result gives the same digest.
- The result of an expression that reads a secret or a sensitive value is
  sensitive too: it is hidden in logs, records, and events. A map or list
  written in YAML keeps that mark per key and per item, so only
  `token: ${{ secrets.api }}` is sensitive in a `with` that also has `url`.

## Rules

- A condition, as `if`, `failed_when`, `changed_when`, `retry.until`, and
  `that` of `assert`, may be written without `${{ }}`, unless it starts
  with `!`.
- `$${{` writes `${{` as it is, as when a task writes a workflow of GitHub
  Actions.
- An expression ends at the first `}}` outside quotes and braces, so a map
  such as `${{ {'a': {'b': 1}} }}` stays whole.
- `${{ }}` is refused in a script of `run`, `service`, and `destroy`, and
  in a `literal` input such as `command.script`: a value goes in through
  `env:` and the script reads `$NAME`.
- `opstruss.cwd`, `opstruss.artifacts`, and `opstruss.temp` are refused in
  units, since what a unit is does not depend on where a command was typed
  or on one run.
- `validate` checks what follows `steps.<id>`, and the outputs that are
  known before the run, and an error names what to write instead: a name
  close to the one written, `units['my-db']` for a name with `-`, or an
  input for `$NAME`, since an expression does not read the environment.
