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
of the catalog, not of OpsTruss. `opstruss.lock` pins the commit that `v1`
names, one pin for every plugin of the catalog, and `opstruss uses update
ops` moves them all at once. A release of the catalog may need a
later OpsTruss or a later version of a tool, as its notes say; an update
that needs a later OpsTruss stops with the version and keeps the pins.

| Plugin | What it runs | Main inputs |
|---|---|---|
| [`tofu`](https://github.com/opstruss/catalog/tree/v1/plugins/tofu) | OpenTofu root modules: an exact plan, apply, and destroy, with the state of the module kept with the state of OpsTruss | `dir`, `vars`, `var_files`, `args` |
| [`terraform`](https://github.com/opstruss/catalog/tree/v1/plugins/terraform) | Terraform root modules, as `tofu` runs OpenTofu ones | `dir`, `vars`, `var_files`, `args` |
| [`compose`](https://github.com/opstruss/catalog/tree/v1/plugins/compose) | Docker Compose projects, named after the unit, with `logs` and `config` tasks | `files`, `profiles`, `env_files`, `args` |
| [`brew`](https://github.com/opstruss/catalog/tree/v1/plugins/brew) | Homebrew formulae and casks; `destroy` uninstalls only what the unit installed | `packages`, `casks`, `args` |
| [`apt`](https://github.com/opstruss/catalog/tree/v1/plugins/apt) | Debian and Ubuntu packages, as root with `sudo: true` on the unit; `destroy` removes only what the unit installed | `packages`, `args` |
| [`dnf`](https://github.com/opstruss/catalog/tree/v1/plugins/dnf) | Fedora, RHEL, and other RPM packages, as root with `sudo: true` on the unit; `destroy` removes only what the unit installed | `packages`, `args` |
| [`restic`](https://github.com/opstruss/catalog/tree/v1/plugins/restic) | restic repositories, which `restic init` makes, with the tasks `backup`, `forget`, `check`, and `restore`, whose outputs later steps read | `repository`, `password`, `credentials`, `args` |
| [`vault`](https://github.com/opstruss/catalog/tree/v1/plugins/vault) | No unit: the task `read` gives secrets of the key/value engine of Vault, for `secrets:` | `path`, `field`, `version` |
| [`ansible`](https://github.com/opstruss/catalog/tree/v1/plugins/ansible) | Ansible playbooks: a plan in check mode by host, and the data of `set_stats` as outputs | `playbook`, `inventory`, `vars`, `limit`, `args` |
| [`mise`](https://github.com/opstruss/catalog/tree/v1/plugins/mise) | The tools of a mise configuration; the plan lists each tool that the machine lacks | `dir`, `args` |
| [`chezmoi`](https://github.com/opstruss/catalog/tree/v1/plugins/chezmoi) | Dotfiles that chezmoi applies from a repository; the plan lists new commits and files edited by hand | `repo`, `prompts`, `args` |

The catalog has components too, used the same way:
[`packages`](https://github.com/opstruss/catalog/tree/v1/components/packages)
installs one list of packages with the package manager of the machine,
brew, apt, or dnf.

The README of each plugin is its documentation: its inputs, outputs,
tasks, and what it needs in `PATH`. To write a plugin of your own, see
[Writing a plugin](./guides/writing-a-plugin).
