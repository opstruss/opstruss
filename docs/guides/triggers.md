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

## The daemon

```sh
opstruss up -d     # start the daemon of the stack, with its triggers
opstruss list      # each task, its triggers, and when it runs next
opstruss runs      # the runs of the stack
opstruss stop      # stop the daemon, its runs, and its services
```

- `up -d` starts the daemon in the background when none runs, and
  installs the triggers. Each later `up` goes to that daemon and installs
  them again from the files.
- The triggers run with the environment of the last `up` that installed
  them. An `up` from a shell without the `.env` of the project would make
  the next scheduled run go without those variables, so load it the same
  way every time, with mise or direnv, which load it per directory.
- A webhook needs `runtime.listen`, and each one a `secret` and a
  `signature`: `github`, `gitlab`, `slack`, `stripe`, `standard`,
  `bearer`, or a header of your own. A request that fails the check gets
  401 and runs nothing. TLS belongs to a reverse proxy, such as Caddy.
  `respond: wait` answers with the outputs of the task.

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
loginctl enable-linger "$USER"   # start without a login
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

`mise x -C <project>` brings the versions of `mise.toml` and its `[env]`,
`OPSTRUSS_STACK` among them. Without mise, give the absolute path of
`opstruss` and `--stack`, and load `.env` with `EnvironmentFile=` in
systemd, or with
`sh -c 'set -a; . ./.env; set +a; exec opstruss up -d'` and a
`WorkingDirectory` in launchd.
