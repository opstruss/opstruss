# Services

A unit with `service:` is a program that keeps running, such as a web
server or a worker. `up` starts it, waits until it is ready, and the
daemon of the stack keeps it running; `destroy` stops it.

<<< @/examples/services/opstruss.yaml

The example needs `python3` and `curl` in `PATH`.

```console
$ opstruss up -d
  + web    new
  + smoke  new

Plan of stack default: 2 to run

Apply web, smoke? [y/N] y
▸ web › up
│ running, pid 87536
✓ web › up: done  1.1s
▸ smoke › up
│ web answers
✓ smoke › up: done  0.1s

Up of stack default: 2 done
```

- `ready` says when the service counts as started: a URL that answers
  with a 2xx status, a port that accepts a connection, or a program that
  exits with 0. `up` waits for it, up to `timeout`, before the units
  that need the service run.
- `sources` are files of the service: when one changes, `up` restarts it.
  A change of its program, `env:`, or inputs does the same.
- `restart` says when the daemon starts it again after it exits:
  `on-failure` by default, `always`, or `no`.

## In the daemon or in the foreground

`up -d` starts the daemon of the stack in the background, which keeps
the services and runs the triggers until `opstruss stop`. Without `-d`,
an `up` that starts a service becomes the daemon itself, in the
foreground: it shows the output of the services, and Ctrl-C stops them.

## Looking at a service

| Command | Shows |
|---|---|
| `opstruss status` | Each service with its pid, since when it runs, and its restarts |
| `opstruss logs web` | What the service wrote; `-f` follows it |
| `opstruss list` | Each unit with its state |
| `opstruss stop` | Stops the daemon and its services |

After a crash of the daemon, the next `up` finds the services that are
left: one that it can prove its own is stopped and started again, and
one that it cannot is kept until `opstruss unlock <unit>` lets it go.
To start the daemon after a reboot, see
[Triggers and the daemon](./triggers#after-a-reboot).
