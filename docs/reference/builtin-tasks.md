# Built-in tasks

Built-in tasks come with the binary. A step or a task calls one by its
short name, `http: {…}`, or its full name, `opstruss.http: {…}`.

| Task | What it does | Main inputs | Outputs |
|---|---|---|---|
| `command` | Runs a program: arguments or a script | `argv`, `script`, `shell`, `stdin` | `stdout`, `stderr`, `exit_code` |
| `http` | Sends an HTTP request, also to a webhook | `url`, `method`, `headers`, `body`, `status_code`, `timeout`, `ca` | `status`, `headers`, `body`, `tls` |
| `read` | Reads a file | `path`, `format` | `content` |
| `write` | Writes a file, only when its content differs | `path`, `content`, `format`, `mode` | `path` |
| `set` | Shapes values without a program | any values | the same values |
| `assert` | Stops when a condition is false | `that`, `msg` | |
| `wait` | Waits for a time, a port, or a URL | `seconds`, `tcp`, `http`, `timeout` | |
| `log` | Writes a message to the log and the events of the run | `msg`, `level` | |
| `kv` | Keeps a small value of the stack between runs | `key`, `value`, `ttl`, `expect` | `value` |

A task that counts its runs and writes a report:

<<< @/examples/builtin-tasks/opstruss.yaml

```console
$ opstruss report
…
▸ report › step 5
│ run 1
✓ report › step 5  0.0s
✓ report  0.0s
```

## command

`run:` is the short form of `command:`: a string is `script`, a list is
`argv`. Exactly one of them is written.

- A script runs with `sh -e`, or `bash --noprofile --norc -eo pipefail`
  for `shell: bash`, as in GitHub Actions, so a command that fails stops
  it; `set +e` in the script turns that off. `python`, `node`, and `pwsh`
  run it as it is, through `python3 -c`, `node -e`, and
  `pwsh -NoProfile -NonInteractive -Command`.
- `script` is a literal input: `${{ }}` is refused in it, as in a string
  of `run`. A value goes in through `env:`, and the script reads `$NAME`.
- `stdout` and `stderr` show as the program writes them, and keep their
  last 64 KiB, enough to check a message or `exit_code`. Data from a
  stream is an output with `from: stdout`, which is not shown and holds up
  to 1 MiB.
- `stdin` takes `{value, format}`, as the `stdin:` of a step.

## http

- `status_code` is a list of statuses or patterns such as `2xx`, which is
  the default; any other status fails the task.
- A response with a JSON `Content-Type` has its body parsed. The names of
  `headers` are in lowercase, and a header that comes more than once is
  joined with `, `. A body is at most 1 MiB.
- A string `body` is sent as it is; any other value is sent as JSON, with
  `Content-Type: application/json` unless `headers` names one.
- An `https://` request also gives `tls`: its `version` and its
  `certificates`, leaf first, each with `subject`, `issuer`, `not_before`,
  `not_after`, and `dns_names`. A check that a certificate lasts two more
  weeks:
  `timestamp(steps.c.outputs.tls.certificates[0].not_after) - opstruss.time > duration('336h')`.
- `ca` adds a CA in PEM to the roots of the system for that request, such
  as `${{ file('root-ca.pem') }}`, for a server with a CA of its own.
  Nothing turns off the check of certificates.
- A method other than `GET` and `HEAD` counts as a change.
- `http` fetches one page; paging belongs to a script or a plugin.

## read and write

- A relative `path` is read from the directory of the file that writes
  the step, or from its `dir:`. `read` takes up to 1 MiB.
- `format` is one of `text`, `json`, `yaml`, `lines`, and `dotenv`.
  Templates of text, such as an nginx.conf, belong to a tool such as
  gomplate or envsubst; `write` gives it the data.
- `write` writes through a temporary file and a rename, so a reader never
  sees half of it, and does not make directories: a missing one fails the
  step.
- `mode` is a string, as `'0644'`, since YAML reads `0644` as an octal
  number. A new file is `'0644'`, and an existing one keeps its mode.
- A sensitive `content` is written with mode `'0600'`, unless `mode` says
  otherwise, and never shows in logs, records, or events.
- `write` changes something only when the content or the mode changes.

## kv

`kv` keeps one JSON value per key in the state of the stack, up to
1 MiB, for what runs remember: an alert sent once an hour, a cursor, a
counter, or the list of previews that events asked for.

- A step with `value` writes, and `value: null` deletes; one without
  `value` reads. `ttl`, such as `1h`, makes the key go after that time.
- Each write holds the lock of its key. With `expect`, the write happens
  only when the value is still `expect`, `null` for a key that is not
  there; otherwise the step fails with the newest value as its output,
  and `retry` tries again with it, as the example above does.
- The expression `kv('<key>')` reads the same memory, also in units, and
  `kv('<prefix>*')` reads every key with that prefix as a map. A
  collection that several runs write takes one key per item, such as
  `preview-pr-12`, so that no writer reads and writes another's item.
- A key starts with a lowercase letter or a digit, and holds lowercase
  letters, digits, `.`, `_`, and `-`; `lowerAscii()` and `replace()` shape
  one from other text.
- A sensitive value is kept encrypted, as records are, and reads as
  sensitive.

## wait, assert, and log

- `wait` waits for exactly one of `seconds`, a `tcp` port, or an `http`
  URL that answers `2xx`, checking once a second until `timeout`. To wait
  for anything else, check it in a step with `retry.until`.
- `assert.that` is one condition or a list, written as `if` is.
- `log.msg` is any value, written as JSON when it is not a string, at
  `level` `debug`, `info` (the default), `warn`, or `error`.

Tools such as OpenTofu and Docker Compose come as [plugins](../plugins),
not as built-in tasks.
