# Contributing

How a change to OpsTruss is made and checked. Everyone who takes part
follows the [Code of Conduct](https://github.com/opstruss/.github/blob/main/CODE_OF_CONDUCT.md),
and a security vulnerability goes to private vulnerability reporting, as
[SECURITY.md](https://github.com/opstruss/.github/blob/main/SECURITY.md)
says, not to an issue.

## Tools

[mise](https://mise.jdx.dev) installs the versions of Go, the linters,
sops, and node that `mise.toml` pins, as CI does:

```sh
mise install
```

The targets of the `Makefile` run through mise, so the pinned versions are
the ones used: `mise x -- make test`.

## Checks

| Target | Runs | When |
|---|---|---|
| `make test` | every test, and the crash scripts in a build with failpoints | every change |
| `make lint` | golangci-lint, `go mod tidy -diff`, actionlint, and `goreleaser check` | every change |
| `make criteria` | the criteria of crashes, locks, load, and the use cases, for up to 30 minutes | a change to the engine, the process list, failpoints, or help |
| `make docs-gen` | the reference pages in `docs/reference/` from the schemas and the commands | a change to `internal/schema/` or to a command |
| `make docs` | the documentation site | a change to `docs/` |

A test of the command line is a script in `testdata/script/<name>.txtar`,
run alone with `go test -run 'TestScripts/<name>' .`. An example on a page
of the documentation is a project in `docs/examples/<name>/`, which
`TestDocsExamples` runs.

## Commits

Commits follow [Conventional Commits](https://www.conventionalcommits.org):
a type such as `feat`, `fix`, `docs`, or `test`, a summary in the
imperative of at most 50 characters, and a body that says why.

After the first release, a commit that changes what an operator, or the
author of a component or a plugin, can notice adds one entry to
[`CHANGELOG.md`](CHANGELOG.md), under `[Unreleased]`, in the same commit:
one or two lines on the effect, with a link to the commit or pull request.
