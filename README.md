# OpsTruss

Event-driven workflow orchestration that runs any tool and hands its typed
results to the next.

- **One file** for the units that a project brings up, such as OpenTofu
  modules, Docker Compose projects, services, or any program, and the
  tasks that operate it.
- **Plan, then apply**: `up` shows what would change and asks once; in
  CI, `--yes` approves.
- **Only what changed runs**, in the order of what each unit reads, and
  the outputs of one tool reach the next as typed values.
- **Triggers** run tasks on a schedule, a webhook, or a change of files,
  and secrets stay hidden in every log.
- **One binary**, with the state of each stack kept on the machine that
  runs it.

## Quick start

Install OpsTruss (below), then:

```sh
mkdir hello && cd hello
opstruss init
```

Replace `opstruss.yaml` with:

```yaml
# opstruss.yaml
opstruss: "1.0"
name: hello
units:
  build:
    run: echo "version=1.2.3" >> "$OPSTRUSS_OUTPUT"
    outputs:
      version: {}
  deploy:
    run: [echo, "deploying ${{ units.build.outputs.version }}"]
tasks:
  greet:
    description: Say hello
    inputs:
      who: {type: string, default: world}
    run: echo "hello $INPUT_WHO"
```

```console
$ opstruss up
  + build   new
  + deploy  new · after build (run)

Plan of stack default: 2 to run

Apply build, deploy? [y/N] y
✓ build › up: done  0.1s
▸ deploy › up
│ deploying 1.2.3
✓ deploy › up: done  0.1s

Up of stack default: 2 done
$ opstruss greet --who Ana
made the age key age1… of this machine in ~/.config/opstruss/age/keys.txt; back it up with .opstruss/state/ of each project, …
▸ greet
│ hello Ana
✓ greet  0.0s
```

`up` runs each unit once its definition changes, in the order of what it
reads, and a task is a command with its inputs as flags. The first run
that keeps a value encrypted, here the input of `greet` for `runs rerun`,
makes the age key of this machine: in `~/.config/opstruss/age/` on Linux,
and in `~/Library/Application Support/opstruss/age/` on macOS. The
[documentation](https://opstruss.github.io) goes on with an OpenTofu
module and two stacks.

## Install

With [mise](https://mise.jdx.dev):

```sh
mise use github:opstruss/opstruss
```

Or download an archive of a [release](https://github.com/opstruss/opstruss/releases)
for Linux or macOS, on amd64 or arm64, or build it with Go:

```sh
go install github.com/opstruss/opstruss@latest
```

## Documentation

[opstruss.github.io](https://opstruss.github.io): getting started, guides,
and the reference of `opstruss.yaml`, plugins, and the command line.

## License

OpsTruss is released under the [MIT License](LICENSE).
