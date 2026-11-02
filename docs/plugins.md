# Plugins

The official plugins live in the
[catalog](https://github.com/opstruss/catalog), which has versions of its
own. Declare it once, and each unit uses a plugin as `ops.<name>`:

```yaml
uses:
  ops: github.com/opstruss/catalog@v1
units:
  infra:
    uses: ops.tofu
    with: {dir: ./infra}
```

Three versions meet here: the OpsTruss that you run; the level in the
`opstruss:` line of a file, the OpsTruss that the file is written for
([compatibility](./reference/compatibility)); and `v1`, the major version
of the catalog, whose plugins below work with OpsTruss 1.0 and later.
`opstruss.lock` pins the commit that `v1` names, one pin for every plugin
of the catalog, and `opstruss plugins update ops` moves them all at once.

| Plugin | What it runs | Main inputs |
|---|---|---|
| [`tofu`](https://github.com/opstruss/catalog/tree/v1/plugins/tofu) | OpenTofu root modules: an exact plan, apply, and destroy, with the state of the module kept with the state of OpsTruss | `dir`, `vars`, `var_files`, `args` |
| [`compose`](https://github.com/opstruss/catalog/tree/v1/plugins/compose) | Docker Compose projects, named after the unit, with `logs` and `config` tasks | `files`, `profiles`, `env_files`, `args` |

The README of each plugin is its documentation: its inputs, outputs,
tasks, and what it needs in `PATH`. To write a plugin of your own, see
[Writing a plugin](./guides/writing-a-plugin).
