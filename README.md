# OpsTruss

Event-driven workflow orchestration that runs any tool and hands its typed
results to the next.

```yaml
# opstruss.yaml
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
▸ greet
│ hello Ana
✓ greet  0.0s
```

`up` runs each unit once its definition changes, in the order of what it
reads, and a task is a command with its inputs as flags. The units can be
OpenTofu modules, Docker Compose projects, services that the daemon of the
stack keeps running, or any program, and tasks run on a schedule, a
webhook, or a change of files.

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

The first release is coming; until then, `@main` builds the latest code.

## Documentation

[opstruss.github.io](https://opstruss.github.io): getting started, guides,
and the reference of `opstruss.yaml`, plugins, and the command line.

## License

OpsTruss is released under the [MIT License](LICENSE).
