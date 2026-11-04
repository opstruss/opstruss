# Environment

The variables that `opstruss` reads. The programs that it runs get
variables of their own, which the [program contract](./program-contract)
lists.

| Variable | Is |
|---|---|
| `OPSTRUSS_STACK` | The stack to use when `--stack` is not given |
| `OPSTRUSS_PARALLEL` | How many units run at once, 10 by default |
| `OPSTRUSS_AGE_KEY` | The age key of this machine as text, tried before its file |
| `OPSTRUSS_AGE_KEY_FILE` | The file of the age key of this machine, instead of `opstruss/age/keys.txt` in the configuration directory |
| `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE` | The age keys of sops, tried after those of OpsTruss to open sensitive values |
| `XDG_CONFIG_HOME` | The configuration directory, also on macOS; without it, `~/.config` on Linux and `~/Library/Application Support` on macOS |
| `GITHUB_TOKEN` | A token for the binaries of plugins that come from the releases of `github.com/opstruss/catalog` |
| `NO_COLOR`, `FORCE_COLOR`, `CLICOLOR_FORCE` | Color, as [Exit codes and JSON](./exit-codes-and-json#color) says |
| `TERM` | `dumb` is no terminal: no color, and no blocks or live lines |
| `GITHUB_ACTIONS` | `true` groups the output of each unit and task, which GitHub Actions folds |
| `OPSTRUSS_RUN`, `OPSTRUSS_RUN_PARENTS` | Set by OpsTruss for the programs of a run: an `opstruss` that one of them starts is a run inside it, and takes its stack from `--stack` or the only stack, not from `OPSTRUSS_STACK` |
| `OPSTRUSS_LOG` | `debug` prints what OpsTruss does, for a bug report; what it prints may change in any version |
