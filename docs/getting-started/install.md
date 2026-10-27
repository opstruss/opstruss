# Install

OpsTruss is one binary, for Linux and macOS on amd64 and arm64. On
Windows it runs in WSL2.

## Requirements

OpsTruss runs programs with `sh`, and fetches the
[catalog](../plugins) with `git`. Each plugin needs the programs of its
tool in `PATH`: `tofu` and `jq` for `tofu`, and `docker` with Compose v2
and `jq` for `compose`. `opstruss doctor` checks them for a project.

## With mise

[mise](https://mise.jdx.dev) pins the version of OpsTruss in the
`mise.toml` of a project, next to the versions of the tools that the
project runs, such as `tofu`:

```sh
mise use github:opstruss/opstruss@0.1.0
```

`mise use -g` installs it for every directory.

## From a release

Every [release](https://github.com/opstruss/opstruss/releases) has an
archive per platform, `checksums.txt`, and a build attestation of each
archive. With the [GitHub CLI](https://cli.github.com):

```sh
version=0.1.0
archive=opstruss_${version}_linux_amd64.tar.gz
gh release download "v$version" --repo opstruss/opstruss --pattern "$archive" --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt
gh attestation verify "$archive" --repo opstruss/opstruss
tar -xzf "$archive" opstruss
mkdir -p ~/.local/bin
install opstruss ~/.local/bin/
```

On macOS, `shasum -a 256 --ignore-missing -c checksums.txt` checks the
archive, and the archive is `opstruss_${version}_darwin_arm64.tar.gz` on
Apple silicon. `~/.local/bin` must be in `PATH`, which macOS does not do
by itself: add `export PATH="$HOME/.local/bin:$PATH"` to the profile of
the shell, or install it with `sudo install opstruss /usr/local/bin/`.

## With Go

Go 1.27 or newer builds it from source:

```sh
go install github.com/opstruss/opstruss@latest
```

## Check

```sh
opstruss version
```

`opstruss --version` prints the same. `opstruss doctor`, in a project,
says whether the programs that the project needs are in `PATH`, and where
the key of this machine is.

## Upgrade

Install the new version the same way, as
`mise use github:opstruss/opstruss@<version>`. Before 1.0, a minor release
can ask something of a project: the
[changelog](https://github.com/opstruss/opstruss/blob/main/CHANGELOG.md)
says so first, under "Upgrade notes". A daemon keeps running the binary
that started it: run `opstruss stop`, then `opstruss up -d`, in each
project whose daemon runs.

## Shell completion

`opstruss --completion <shell>` prints the completion script for bash,
zsh, or fish, which completes commands, the tasks of the project, units,
stacks, and runs:

```sh
# bash, in ~/.bashrc
source <(opstruss --completion bash)
# zsh, in ~/.zshrc
source <(opstruss --completion zsh)
# fish
opstruss --completion fish > ~/.config/fish/completions/opstruss.fish
```

## Editor support

The schema of `opstruss.yaml` gives completion and checks in editors
that use the YAML language server, such as VS Code with the YAML
extension. `opstruss init` writes the line that names it at the top of
the file, with the schema of its version:

```yaml
# yaml-language-server: $schema=https://opstruss.github.io/v0.1/schema/project.json
```

The schema of `opstruss.plugin.yaml` is `plugin.json` in the same place.

## Files and directories

| Path | Holds | Back up |
|---|---|---|
| `.opstruss/` of a project | the state of its stacks, the records and logs of its runs, the daemon of each stack | `.opstruss/state/` |
| `~/Library/Application Support/opstruss/age/keys.txt` on macOS, `~/.config/opstruss/age/keys.txt` on Linux | the age key of this machine, which opens the sensitive values of the state; `$OPSTRUSS_AGE_KEY_FILE` names another file | yes |
| `~/.opstruss/` | the cache of fetched plugins, and the id of this machine | no |

`opstruss doctor` shows where the age key is.

## Uninstall

Take the stacks down first, with `opstruss destroy --stack <stack>` in
each project, and stop their daemons with `opstruss stop`. Then remove
the binary (`mise uninstall github:opstruss/opstruss`, or the file you
installed) and `~/.opstruss/`. The age key and `.opstruss/` of each
project are the only way to open what the state keeps; remove them only
when no stack of any project needs them.

## Without mise

mise is one way to put OpsTruss and the tools of a project in `PATH`, and
to load a `.env` file. OpsTruss needs neither: any package manager can
install the tools, since `requires:` only checks that a program is in
`PATH`, and `doctor` shows where each one is. The pages that use mise say
what to do without it.
