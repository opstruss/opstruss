# Writing a plugin

A plugin wraps a tool, so that a unit uses it with a few inputs instead
of a script: it plans, applies, and destroys, and its outputs go to the
next unit. A unit with `run:` is enough for a script that one project
needs once; a plugin is for a tool that several units or projects use.

This page writes `note`, a plugin whose unit is one text file.

## The files

A plugin is a directory with `opstruss.plugin.yaml` and its programs:

```text
plugins/note/
├── opstruss.plugin.yaml
└── note.sh
```

<<< @/examples/writing-a-plugin/plugins/note/opstruss.plugin.yaml

- `inputs` are what a unit gives through `with:`, checked against their
  types before any program runs.
- `outputs` are what `up` writes, which other units read as
  `units.<name>.outputs.<key>`.
- `tasks` holds the programs. A relative program, as `./note.sh`, is read
  from the directory of the plugin; make it executable, with
  `chmod +x note.sh`.

The [reference of opstruss.plugin.yaml](../reference/plugin-yaml) has
every key.

## The program

Each program starts in the directory of the file that writes the unit,
with each input as `INPUT_<NAME>`:

<<< @/examples/writing-a-plugin/plugins/note/note.sh

- `plan` changes nothing. It writes what `up` would change to
  `$OPSTRUSS_PLAN`; no changes means the unit is unchanged. A plan that
  writes nothing means the unit runs.
- `up` makes the file and writes the output `path` to `$OPSTRUSS_OUTPUT`.
  It must be safe to run again, also after an attempt that did not finish.
- `destroy` takes the file away, and succeeds when it is already gone.
  The record of the unit keeps this program and the inputs of the last
  `up`, so `destroy` works after the unit leaves the files.

The [program contract](../reference/program-contract) has everything a
program gets and gives: `OPSTRUSS_STATE` with the last outputs,
`OPSTRUSS_STATE_KEY` to tag what it makes, `OPSTRUSS_REASON`, and the
digest that makes a plan exact.

## Use it

<<< @/examples/writing-a-plugin/opstruss.yaml

```console
$ opstruss plan
  + readme  new · create notes/readme.txt

Plan of stack default: 1 to run
```

Once `up` wrote the file, `plan` says `unchanged`, until the file or the
text differs; `destroy` removes it.

## Standard tasks and hooks

| Task | Runs when | Does |
|---|---|---|
| `plan` | `plan`, and `up` before it applies | Changes nothing; writes `$OPSTRUSS_PLAN` |
| `up` | `up`, for a unit whose definition or plan changed | Brings the unit to what its inputs say; writes the outputs |
| `destroy` | `destroy`, and `up` for a unit that left | Takes the unit away |
| `status` | `opstruss status` | Reads the state of what the unit made |
| `logs` | `opstruss logs`; with an input `follow` for `-f` | Prints its logs |
| `exec` | `opstruss exec <unit> -- <program>` | Sets up the tool, then runs `exec "$@"` |
| `watch` | `on: watch: {unit: …}` of a task | Writes a line per event |

- A plugin with `up` can be the `uses:` of a unit. Without `plan`, the unit
  runs when its definition changes; without `destroy`, `destroy` forgets
  its record.
- Any other name is a task of the plugin, which a step calls as
  `note.<task>: {…}` and a unit uses as `uses: note.<task>`.
- `plan`, `up`, and `destroy` run only for a unit, under its lock, so a
  step cannot call them; a step runs `task: up` with `units:` instead.

## Variables and programs

- `requires:` names the programs that must be in `PATH`. `opstruss
  doctor` shows where each one is, and a plan fails, with the name, for a
  unit whose program is missing.
- A program gets no variable of the environment of OpsTruss unless the
  plugin asks for it in `env:`, by name or by pattern, and the project
  allows it with `pass:`. A plugin that runs the files of its user, as
  `tofu` does, asks for `*`.
- `bin:` downloads a binary per platform, with its SHA-256, into `PATH`
  of the programs of the plugin.
- `state: tool` gives a tool that keeps its own state, as OpenTofu does,
  the directory `$OPSTRUSS_STATE_DIR`, which moves and locks with the
  state of OpsTruss.

## Share it

A plugin is used by its path: in the project, as above, or in a checkout
of another repository next to it, such as a git submodule. This version
fetches only `github.com/opstruss/catalog` by itself, so the plugins of
other repositories come in that way.
