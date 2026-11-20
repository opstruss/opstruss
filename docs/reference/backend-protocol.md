# Backend protocol

A plugin backend keeps the state of OpsTruss somewhere other than
`.opstruss/state/`, such as a bucket that a team and CI share. It is a
program that `backend.run` of its `opstruss.plugin.yaml` names, and a
project uses it through `backend:`. This page is what such a program can
rely on, since 1.1.

```yaml
# opstruss.plugin.yaml
opstruss: "1.1"
name: s3
requires: [aws, jq]
inputs:
  bucket: {type: string, required: true}
backend:
  run: [./s3.sh]
```

## The program

- OpsTruss starts the program when a command opens the state, once for
  each backend, and closes its stdin when the command is done with it.
  It also stops one that sat unused for a minute, and starts it again for
  the next operation. A program that does not end 10 seconds after its
  stdin closes gets `SIGTERM`, then `SIGKILL`.
- It gets the environment of a [program](./program-contract#environment)
  that runs outside a run: the inputs that `with:` of `backend:` gives, as
  `INPUT_<NAME>` only, so no file with a credential outlives an OpsTruss
  that was killed, `OPSTRUSS_PLUGIN_DIR`, `OPSTRUSS_PROJECT`,
  `OPSTRUSS_STACK`, the stack of the command, and `OPSTRUSS_CONTRACT`. It
  gets no facts of a run, since it also serves commands without one, such
  as `opstruss state`.
- stdin and stdout carry the protocol; what the program writes to stderr
  is a warning of the run, or of the command, with the values of secrets
  hidden. An operation that fails says why in the `message` of its error.
- An answer that does not come within 5 minutes stops the program, and
  that operation, as every later one of the command, fails.

## Messages

JSON-RPC 2.0, one message a line, as UTF-8. OpsTruss sends one request,
waits for its answer, and sends the next; the first is `initialize`.

| Method | Params | Result | Errors, in `error.data.code` |
|---|---|---|---|
| `initialize` | `protocol: 1` | `protocol: 1`, `location`, `capabilities` | |
| `get` | `key` | `data`, `etag` | `not_found` |
| `put` | `key`, `data`, and `if_match` or `if_none_match: true` | `etag` | `conflict` |
| `delete` | `key`, `if_match` | `{}` | `conflict`, `not_found` |
| `list` | `prefix` | `keys` | |
| `scan` | `prefix` | `items`, each with `key` and `data` | |
| `lock` | `key`, `holder` | `token` | `locked`, with `holder` and `token` |
| `unlock` | `key`, `token` | `{}` | `conflict`, `not_found` |

```text
→ {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocol":1}}
← {"jsonrpc":"2.0","id":1,"result":{"protocol":1,"location":"s3://acme-opstruss/platform/","capabilities":{"scan":true}}}
→ {"jsonrpc":"2.0","id":2,"method":"get","params":{"key":"prod/_meta"}}
← {"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"no such key: prod/_meta","data":{"code":"not_found"}}}
```

- `location` names where the state is, without credentials: it shows in
  messages, and with the source of the plugin it is what makes two
  backends the same one. An answer with another `protocol` stops the
  command.
- `capabilities` names the methods that a program may leave out and
  serves: `{"scan": true}`, or `{}`. OpsTruss sends no method that it
  does not name.
- A key is a path relative to the location, such as `prod/_meta`,
  `prod/web/state.json`, `prod/web/state.lock`, or `prod/_kv/<name>`, and
  its `data` is JSON text.
- `put` replaces the whole of a key. With `if_match`, it writes only when
  the key still has that etag; with `if_none_match`, only when there is
  no key; otherwise the error is `conflict`. `delete` takes `if_match` the
  same way. An etag is opaque to OpsTruss, and changes with each write.
- `get` and `list` see each write that finished, from any client.
- `list` gives every key that starts with `prefix`, at any depth, in any
  order. A lock that is held is a key there, and one that was let go is
  not: a plan sees which units another run holds in what it listed,
  without another operation.
- `scan` gives the keys that `list` gives, each with its `data`, so a plan
  reads a stack in one operation rather than a `get` for each record. A
  key that goes while it scans, such as a lock that was let go, may be
  left out. It is no snapshot of one moment and has no etags: OpsTruss
  writes nothing from it, and reads a key with `get` before it writes.
- `lock` does not wait: on a key that is locked, it answers `locked`
  with the `holder` that the lock keeps, the object OpsTruss gave, and its
  `token`. A lock stays when its holder dies, so another process, as
  `opstruss unlock`, can let it go. `unlock` lets go only with the token
  of the lock: `conflict` for another token, `not_found` for a lock that
  is not there. A token is of one lock alone: a lock taken again, also
  by the same holder, gets another token, so an unlock that read the
  token of the lock before does not let it go. A program whose token
  comes from the content of an object, as an ETag of S3, keeps a random
  value in each lock.
- Any other error fails the operation, with its `message`. A method that
  the program does not know is error `-32601`, so a later version can add
  methods as capabilities.

## The probe

`opstruss init` and `opstruss doctor` try the contract before a stack
starts in a backend, on keys under `_probe/<id>/` that no stack reads:

1. `put` with `if_none_match` of a key as deep as a record,
   `_probe/<id>/unit/state.json`, then `get` gives what it wrote, with its
   etag, `list` of the prefix gives the key, and `scan` gives it with what
   was written; a second `put` with `if_none_match` is a `conflict`.
2. `put` with the etag writes, and `get` gives what it wrote, with its new
   etag; `put` and `delete` with the old etag are a `conflict`.
3. `delete` with the etag, then `get` is `not_found`.
4. A second `lock` is `locked`, with the holder and the token of the
   first, and `list` and `scan` give the key of the lock.
5. `unlock` with another token is a `conflict`; with the token, the lock
   goes, and the same holder takes it again with another token.
6. `list` and `scan` of the prefix are empty.

The steps of `scan` run only for a program that names it in
`capabilities`. A storage that fails a step, such as one that deletes an object whose
etag does not match, is refused with the step, before any state is
written there.
