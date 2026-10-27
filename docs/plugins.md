# Plugins

The official plugins live in the
[catalog](https://github.com/opstruss/catalog), which has versions of its
own. Declare it once, and each unit uses a plugin as `cat.<name>`:

```yaml
uses:
  cat: github.com/opstruss/catalog@v1
units:
  infra:
    uses: cat.tofu
    with: {dir: ./infra}
```

`opstruss.lock` pins the commit that `v1` names, and `opstruss plugins
update` moves the pin.

| Plugin | What it runs | Main inputs |
|---|---|---|
| [`tofu`](https://github.com/opstruss/catalog/tree/v1/plugins/tofu) | OpenTofu root modules: an exact plan, apply, and destroy, with the state of the module kept with the state of OpsTruss | `dir`, `vars`, `var_files`, `args` |
| [`compose`](https://github.com/opstruss/catalog/tree/v1/plugins/compose) | Docker Compose projects, named after the unit, with `logs` and `config` tasks | `files`, `profiles`, `env_files`, `args` |

The README of each plugin is its documentation: its inputs, outputs,
tasks, and what it needs in `PATH`. To write a plugin of your own, see
[Writing a plugin](./guides/writing-a-plugin).
