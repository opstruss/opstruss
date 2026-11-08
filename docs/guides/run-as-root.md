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
the terminal and hands it to `sudo -v`, keeping it only to hide it from
the output of the run, then keeps the ticket of sudo while the run lasts.

sudo keeps that ticket for the terminal, not for one program: while it
lasts, a program of the run, or of that terminal, that runs `sudo` itself
needs no password either, as after `sudo` at a shell. `sudo: true`
chooses what OpsTruss starts as root; it does not keep other programs
from sudo, so a plugin or a component from git is code that you trust.
A program that runs `sudo` by itself is not part of what OpsTruss
promises.

A run without a terminal, as in CI, the daemon, or a trigger, has no
ticket to use and fails before the program starts; there, let sudo run
OpsTruss without a password:

```text
# /etc/sudoers.d/opstruss
deploy ALL=(root) NOPASSWD: /usr/local/bin/opstruss __sudo
```

`opstruss __sudo` runs whatever program OpsTruss gives it, so this line
lets `deploy` run any program as root without a password; it is as much as
root itself. `opstruss doctor` says whether sudo needs a password, with the
line for the path of the binary that runs. sudo matches the file that the
line names, so a link that stays across upgrades, such as
`/usr/local/bin/opstruss`, keeps it working when the binary is in a
directory of its version.

## What the program gets

- `HOME` and `USER` of root, unless `env:` sets them, and `SUDO_USER`,
  `SUDO_UID`, and `SUDO_GID`; the rest of its environment is as it is
  without sudo, so sudoers needs no `SETENV`.
- Its stdin, its output, and the files of the
  [program contract](../reference/program-contract.md) as without sudo.
  Files that it makes in `OPSTRUSS_TEMP`, `OPSTRUSS_ARTIFACTS`,
  `OPSTRUSS_WORKDIR`, and `OPSTRUSS_STATE_DIR` go back to the operator once
  it ends; files elsewhere stay root's, and so do a file there with another
  link, which may be a file elsewhere, and what is on another filesystem.
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
