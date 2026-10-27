# CI with GitHub Actions

::: warning The state lives on one machine
This version keeps the state of each stack in `.opstruss/` of the
machine that runs it. A GitHub-hosted runner starts empty every time:
it has neither the records of the units nor the state of the OpenTofu
modules, so an `up` there would make every resource again. Run `up`
only on a self-hosted runner on the machine that holds the state, and
use GitHub-hosted runners to check the files. A shared state comes in a
later version.
:::

## A workflow

<<< @/examples/ci/.github/workflows/opstruss.yml

- `validate` checks every file and every stack without running
  anything, and with `--locked` fails when `opstruss.lock` would have to
  change, as `npm ci` does.
- `up` runs on the machine of the state, in a checkout of the project at
  a fixed path that the runner does not clean. A checkout made by
  `actions/checkout` would be cleaned with `git clean -ffdx`, which
  deletes `.opstruss/`, since `.gitignore` leaves it out.
- `concurrency` keeps two runs of the workflow from waiting on the locks
  of the same stack.

## Approval and exit codes

At a terminal, `up` asks before it changes anything. In CI nothing
answers:

- Without `--yes`, `up` stops at the plan of a unit that would change,
  and exits with 2, which a workflow can treat as "needs approval".
- With `--yes`, it applies the changes of that run, as `terraform apply
  -auto-approve` does.
- 1 means that something failed; [Exit codes and JSON](../reference/exit-codes-and-json)
  has the rest.

A gate that a person approves fits in a GitHub environment with required
reviewers: one job runs `opstruss plan --stack prod`, and the job with
`up --yes` waits for the approval of its environment.

## Output in GitHub Actions

In GitHub Actions, OpsTruss prints in color, and puts the lines of each
program in a group that folds, `::group::`, so the lines of OpsTruss
itself stay in view. `--json` prints one event per line instead, for a
step that reads them.

## Without mise

On a GitHub-hosted runner, the release archive and its checksums install
it:

```sh
version=0.1.0
gh release download "v$version" --repo opstruss/opstruss \
  --pattern "opstruss_${version}_linux_amd64.tar.gz" --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar -xzf "opstruss_${version}_linux_amd64.tar.gz" opstruss
sudo install opstruss /usr/local/bin/
```

`gh` needs `GH_TOKEN: ${{ github.token }}` in the `env:` of that step.
