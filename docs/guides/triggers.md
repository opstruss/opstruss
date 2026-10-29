# Triggers and the daemon

A task with `on:` runs by itself: on a schedule, at an interval, on a
webhook, when files change, or after another task. The daemon of the
stack runs those triggers.

<<< @/examples/triggers/opstruss.yaml

| Trigger | Runs the task |
|---|---|
| `schedule` | At the times of a cron expression, in the time zone of the machine unless `timezone` names another; with `persistent: true`, once for what was missed while the daemon was down |
| `every` | At an interval, such as `5m` |
| `webhook` | On an HTTP request to the daemon whose signature checks out |
| `watch` | On changes of `paths`, on lines of a service (`unit:`), or on lines of a program |
| `after` | After a run of another task or command ends, by its `outcome` |

`if:` reads `event`, which says what happened, such as `event.paths` of
a `watch` or `event.body` of a webhook; a run whose `if` is false is
skipped. `batch` and `debounce` gather the events of a `watch` into one
run.

## What a run gets in `event`

Every run has `event.time`, when it was due or its event came, and
`event.trigger`, what made it: `schedule`, `every`, `webhook`, `watch`,
`after`, `cli`, or `mcp`. The other fields come from its trigger:

| Trigger | `event` has |
|---|---|
| `schedule`, `every` | Nothing more |
| `webhook` | `method`, `path`, `headers` (lowercase names), `query`, and `body`, parsed when it is JSON |
| `watch` of a service (`unit:`) | `unit`, `stream`, and `line`; with `batch` or `debounce`, `lines`, `count`, and `truncated` instead of `line` |
| `watch` of `paths` | `paths`, each `{path, op}` with `op` `create`, `write`, `remove`, or `rename`, and `count`; with `batch` or `debounce`, also `truncated` |
| `watch` of a program | `line`, and `data` when the line is JSON |
| `after` | `task`, `unit`, `outcome`, and `outputs` of the run it follows |

`validate` refuses a field that no trigger of the task gives, such as
`event.run`. A task that also runs from the command line reads a field
with `has(event.<field>)`, or gets its value through `with:`.

## The daemon

The webhook of `deploy` checks its requests with a secret from the
environment, so set it first:

```sh
export GITHUB_WEBHOOK_SECRET=…
opstruss up -d     # start the daemon of the stack, with its triggers
opstruss list      # each task, its triggers, and when it runs next
opstruss runs      # the runs of the stack
opstruss stop      # stop the daemon, its runs, and its services
```

- `up -d` starts the daemon in the background when none runs, and
  installs the triggers. Each later `up` goes to that daemon and installs
  them again from the files.
- When a trigger cannot be installed, such as a webhook whose secret is
  not set, `up` says why and exits with 1, and the daemon keeps the
  triggers it had: none, for a daemon that just started. `list` says so
  too, and says when no daemon runs the triggers at all.
- The triggers run with the environment of the last `up` that installed
  them. An `up` from a shell without the `.env` of the project would make
  the next scheduled run go without those variables, so load it the same
  way every time, with mise or direnv, which load it per directory.
- A webhook needs `runtime.listen`, and each one a `secret` and a
  `signature`: `github`, `gitlab`, `slack`, `stripe`, `standard`,
  `bearer`, or a header of your own. A request that fails the check gets
  401 and runs nothing. TLS belongs to a reverse proxy, such as Caddy.
  The answer to a request that checks out:

  | When | Status and body |
  |---|---|
  | `respond: accepted`, the default | 202, `{"run": "<id>"}` once the run starts |
  | `respond: wait` | 200, or 500 when the run fails: `{"run", "outcome", "exit_code", "outputs"}` |
  | `if:` is false | 202, `{}`, and no run |
  | `with:` gives an input that the task refuses | 422, `{"error": "<why>"}` |
  | The task has too many runs waiting | 503 |

## After a reboot

The daemon does not start by itself after a reboot. A service manager
starts it with `up -d`, which starts the daemon and exits.

With systemd, as a user unit in `~/.config/systemd/user/shop.service`:

```ini
[Unit]
Description=OpsTruss daemon of shop

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/mise x -C /srv/shop -- opstruss up -d
ExecStop=/usr/bin/mise x -C /srv/shop -- opstruss stop
SuccessExitStatus=2

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now shop
sudo loginctl enable-linger "$USER"   # start without a login
```

`SuccessExitStatus=2` keeps the daemon when `up` stops at a change that
needs approval.

With launchd on macOS, in `~/Library/LaunchAgents/dev.shop.opstruss.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.shop.opstruss</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/mise</string>
    <string>x</string>
    <string>-C</string>
    <string>/Users/me/shop</string>
    <string>--</string>
    <string>opstruss</string>
    <string>up</string>
    <string>-d</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>AbandonProcessGroup</key>
  <true/>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.shop.opstruss.plist
```

`AbandonProcessGroup` keeps the daemon when mise exits, so `launchctl
bootout gui/$(id -u)/dev.shop.opstruss` only keeps it from starting at the
next login; `opstruss stop` stops it.

`mise x -C <project>` brings the versions of `mise.toml` and its `[env]`,
`OPSTRUSS_STACK` among them. Without mise, give the absolute path of
`opstruss` and the project with `-C`, as in `ExecStart=/usr/local/bin/opstruss
-C /srv/shop up -d`, and the stack with `--stack` or in `.env`. Load
`.env` with `EnvironmentFile=` in systemd, or with
`sh -c 'set -a; . ./.env; set +a; exec opstruss up -d'` and a
`WorkingDirectory` in launchd.
