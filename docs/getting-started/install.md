# Install

OpsTruss is one binary, for Linux and macOS on amd64 and arm64. On
Windows it runs in WSL2.

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
install opstruss ~/.local/bin/
```

On macOS, `shasum -a 256 --ignore-missing -c checksums.txt` checks the
archive, and the archive is `opstruss_${version}_darwin_arm64.tar.gz` on
Apple silicon.

## With Go

Go 1.27 or newer builds it from source:

```sh
go install github.com/opstruss/opstruss@latest
```

## Check

```sh
opstruss version
```

`opstruss doctor`, in a project, says whether the programs that the
project needs are in `PATH`, and where the key of this machine is.

## Upgrade

Install the new version the same way, as
`mise use github:opstruss/opstruss@<version>`. Before 1.0, a minor release
can ask something of a project: the
[changelog](https://github.com/opstruss/opstruss/blob/main/CHANGELOG.md)
says so first, under "Upgrade notes".

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
extension. A line at the top of the file names it:

```yaml
# yaml-language-server: $schema=https://opstruss.github.io/schema/project.json
```

The schema of `opstruss.plugin.yaml` is `plugin.json` in the same place.

## Without mise

mise is one way to put OpsTruss and the tools of a project in `PATH`, and
to load a `.env` file. OpsTruss needs neither: any package manager can
install the tools, since `requires:` only checks that a program is in
`PATH`, and `doctor` shows where each one is. The pages that use mise say
what to do without it.
