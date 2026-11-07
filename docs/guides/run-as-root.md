# Run as root

A program that changes the system, such as one that installs packages or
writes a file in `/etc`, runs as root with `sudo: true`.

<<< @/examples/sudo/opstruss.yaml

- On a unit with `run`, its program and its `destroy` run as root; on a
  task or a block, its program and each of its steps, unless a step writes
  `sudo: false`.
- A plugin or a component from git asks for it in its own files, and its
  programs run as root only when the unit, the step, or the declaration in
  `uses:` that uses it writes `sudo: true`. `opstruss validate` names what
  asks and is not allowed.
- Everything else runs as the operator, so `opstruss plan` asks for
  nothing unless the plan of a plugin runs as root.

## The password

The first program that runs as root starts through `sudo -n`, once the
plan is approved. When sudo needs a password, OpsTruss asks for it once at
the terminal, hands it to `sudo -v`, and forgets it, then keeps the ticket
of sudo while the run lasts. A run without a terminal, as in CI, the
daemon, or a trigger, fails before the program starts; there, run `sudo
-v` in the same terminal first, or let sudo run OpsTruss without a
password:

```text
# /etc/sudoers.d/opstruss
deploy ALL=(root) NOPASSWD: /usr/local/bin/opstruss __sudo
```

`opstruss __sudo` runs whatever program OpsTruss gives it, so this line
lets `deploy` run any program as root without a password; it is as much as
root itself. `opstruss doctor` says whether sudo needs a password, with the
line for the path of the binary that runs.

## What the program gets

- `HOME` and `USER` of root, unless `env:` sets them, and `SUDO_USER`,
  `SUDO_UID`, and `SUDO_GID`; the rest of its environment is as it is
  without sudo, so sudoers needs no `SETENV`.
- Its stdin, its output, and the files of the
  [program contract](../reference/program-contract.md) as without sudo.
  Files that it makes in `OPSTRUSS_TEMP`, `OPSTRUSS_ARTIFACTS`,
  `OPSTRUSS_WORKDIR`, and `OPSTRUSS_STATE_DIR` go back to the operator once
  it ends; files elsewhere stay root's.
- Ctrl-C, `stop.grace`, and `timeout` stop it as any program, and it stops
  when OpsTruss dies.

sudoers with `log_input` takes away the socket that OpsTruss talks to the
program through; turn it off for OpsTruss with `Defaults!/usr/local/bin/opstruss
!log_input`.

## OpsTruss as root

OpsTruss that runs as root, as in a container or on a server, runs every
program as root, and those with `sudo: true` without sudo. Run it so only
when every program of the project is meant to run as root, and always so:
`sudo opstruss up` leaves `.opstruss` owned by root, which a later run as
the operator cannot open, and sudo gives OpsTruss an environment of its
own, without the variables that `pass:` lets through and with a `PATH`
that may not find the tools of the operator.
