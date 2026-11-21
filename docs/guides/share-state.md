# Share the state with a team and CI

Each stack keeps its records in `.opstruss/state/` of the machine that
runs it, unless the project names a plugin backend. With one, as `s3`
from the catalog, every checkout of the project, on a laptop or in CI,
reads and locks the same records, so a unit that one made, another finds.

## A backend for the project

```yaml
opstruss: "1.1"
name: platform

uses:
  ops: github.com/opstruss/catalog@v1

backend:
  uses: ops.s3
  with:
    bucket: acme-opstruss
    prefix: platform/
    region: ap-southeast-1
  pass: [AWS_PROFILE]

stacks:
  dev:
    backend: local       # dev stays on each machine
  prod: {}
```

- `backend:` at the top is the backend of every stack, and of the global
  scope; a stack may name its own, or `backend: local`.
- `with:` is read before the state opens, so it reads only inputs, secrets
  from `sops` or `env`, and locals of those; so do the `sops` and `field`
  of a secret that it reads. `init` needs only the inputs that `with:`
  reads.
- A project with [global units or tasks](./structure-a-project) and a
  stack with a plugin backend keeps the global scope in a plugin backend
  too: write `backend:` at the top rather than in the stacks.

## Start a stack there

A stack starts in a plugin backend with `init`, never with a first run:

```sh
opstruss init --stack prod    # checks the bucket, then writes prod/_meta there
opstruss init                 # the same for the global scope, when the project has global units or tasks
```

`init` first runs a probe of the backend: a few writes, deletes, and
locks on keys of its own, which a storage that ignores a condition fails.
A run before `init` stops, with the location it looked at, so a bucket or
a prefix that is wrong never starts a second, empty copy of a stack that
would make every resource again.

Another checkout needs no `init`. Its first command finds the state there
and keeps the backend in `.opstruss/state/<stack>/backend.json`; when the
project later names another place, the commands of that checkout stop and
name `opstruss state migrate`.

## In CI

```yaml
secrets:
  key: {env: AWS_ACCESS_KEY_ID}
  secret: {env: AWS_SECRET_ACCESS_KEY}
  session: {env: AWS_SESSION_TOKEN}

backend:
  uses: ops.s3
  with:
    bucket: acme-opstruss
    prefix: platform/
    region: ap-southeast-1
    access_key_id: ${{ secrets.key }}
    secret_access_key: ${{ secrets.secret }}
    session_token: ${{ secrets.session }}
```

Credentials that change each session come through secrets, which stay
out of every record and log; `pass:` is for what names the target, as a
profile. A runner that starts empty runs `up` as a laptop does: the
records, the locks, and the memory of `kv` are in the bucket.

## Locks

A run locks each unit that it changes in the backend, and another run
waits for it, from any machine. A plan reads the records and the locks of
a stack at once and shows a unit that another run holds as `locked by`
that run, without waiting; `up` takes no lock of a unit that it leaves as
it is. A lock in a bucket stays when its holder dies:

- A holder that died on this machine is taken over by the next run, which
  says so.
- One of another machine stays until `opstruss unlock <unit>` lets it go,
  after a question with the age of the lock, or with `--yes` in CI. A
  holder of this machine that still runs keeps its lock.

```sh
opstruss unlock web --stack prod        # the lock of a unit
opstruss unlock _daemon --stack prod    # the lease of the daemon
```

## Daemons

The daemon of a stack whose state is shared holds the lease of the
stack, `_daemon`, while it runs: only that daemon installs the triggers
and keeps the services, so a schedule runs once. A daemon of another
checkout runs what it is asked, as an `up`, and says which machine holds
the lease. A daemon keeps the backend that it took the lease from: once
`opstruss.yaml` keeps the state of its stack in another backend, its
runs stop until it starts again, with `opstruss stop` and then `up`. A
service runs from the checkout whose daemon started it, and an `up` that
writes its record, whatever the unit is now, or a `destroy` of it from
another checkout, on that machine or another, is refused with its
directory and machine.
