# Secrets with sops

A secret is a value that OpsTruss reads when a unit or task needs it,
keeps only in memory, and hides as `***` wherever it writes: the
terminal, logs, records, and events.

## Where secrets come from

| Source | Reads |
|---|---|
| `sops: <file>` | A file encrypted with [sops](https://getsops.io); a dotenv, JSON, YAML, or INI file is a map, and `field:` picks one value |
| `env: <name>` | A variable of the environment of OpsTruss, as in CI |
| `run: <program>` | The standard output of a program, such as `op read` of 1Password |
| `<plugin>.<task>: {…}` | A task of a plugin with an output `value` |

A project with one encrypted file per stack and a token from the
environment, which the next section encrypts and runs:

<<< @/examples/secrets/opstruss.yaml

- A secret reaches a program through `env:`, never inside a script: the
  script reads `$CLOUDFLARE_API_TOKEN`.
- `secrets.infra` is the whole decrypted file, so
  `secrets.infra.CLOUDFLARE_API_TOKEN` is one of its variables.
  `${{ stack }}` in the path picks the file of the stack.
- A secret is read once per run, and only when what runs reads it, so a
  unit that needs no secret runs without them.
- What is computed from a secret is sensitive too, and so is an output
  that a plugin marks sensitive. `opstruss outputs` shows such an output
  as `***`, and `--show-sensitive` shows it as it is.

## Encrypt a file with sops

sops encrypts for keys such as age, AWS KMS, or GCP KMS. This needs `sops`
and `age` in `PATH`, for example with `mise use sops age`. Make an age key,
and tell sops where it is:

```sh
mkdir -p ~/.config/sops/age
age-keygen -o ~/.config/sops/age/keys.txt
export SOPS_AGE_KEY_FILE=~/.config/sops/age/keys.txt
```

`age-keygen` prints the public key, `Public key: age1…`. Without
`SOPS_AGE_KEY_FILE`, sops looks for the key in
`~/.config/sops/age/keys.txt` on Linux but in
`~/Library/Application Support/sops/age/keys.txt` on macOS, so set it in
the profile of the shell, or in the `[env]` of `mise.toml`, for every
command. A `.sops.yaml` at the root of the project says which keys
encrypt which files; write the public key in it:

```yaml
creation_rules:
  - path_regex: ^secrets/
    age: age1…
```

Then write the values of the stack `dev` and encrypt them in place:

```sh
mkdir -p secrets
cat > secrets/dev.env <<'EOF'
CLOUDFLARE_API_TOKEN=…
HCLOUD_TOKEN=…
EOF
sops encrypt -i secrets/dev.env
```

The other secret comes from the environment, so set it, and run:

```console
$ export DEPLOY_TOKEN=…
$ opstruss up
…
│ zone token ***
…
│ deploy token ***
```

OpsTruss runs `sops` from the root of the project with the environment
of the operator, so `.sops.yaml` and the keys that sops finds apply. The
encrypted file can go into git.

## Variables for the tools in plugins

The `tofu` and `compose` plugins run your own modules and Compose files,
which read variables of their own, such as `AWS_PROFILE` or `TF_VAR_zone`.
They ask for every variable, and `pass:` of the project says which reach
them; without `pass:`, none does. One pattern in the declaration covers
every unit that uses it. `*` alone is refused there and in a unit, since
the digest of a unit would then change with each shell, as `PWD` does:

<<< @/examples/tool-variables/opstruss.yaml

Variables in a `.env` file reach OpsTruss through the environment:

```toml
# mise.toml
[env]
_.file = ".env"
```

Without mise, direnv loads it with `dotenv` in `.envrc`, and a shell with
`set -a; . ./.env; set +a`. Load it the same way for every command, since
each `up` that the daemon runs also installs its triggers again with the
variables of that `up`.

`pass:` is for what names the target and the identity, such as
`AWS_PROFILE`, `AWS_REGION`, or `DOCKER_HOST`. The record of a unit keeps
a fingerprint of what it passed, so a plan with another `AWS_PROFILE`
stops, rather than destroy later in another account:

```text
default/dns: error: context changed: AWS_PROFILE differs from the record, whose destroy reaches the old target; up --force moves the unit and forgets that target
```

A credential that changes every session, as `AWS_SESSION_TOKEN` from
`aws-vault exec` or OIDC in CI, would stop every plan that way. Give it as
a secret through `env:` instead, where a new value only runs the unit
again:

<<< @/examples/session-credentials/opstruss.yaml

A plugin that asks for a few variables by name warns in `plan` and
`opstruss doctor` about an item of `pass:` that reaches nothing it asks
for:

```text
default/dns: pass: AWS_PROFILE does not reach plugin x, which does not ask for it
```

## The data key and the key of this machine

Sensitive values in the state, such as an output that a secret made, are
encrypted with the data key of the stack, which the age key of this
machine opens. OpsTruss makes that key the first time it needs it, and
`opstruss doctor` shows where it is. Back it up together with
`.opstruss/state/`: on one machine, they are the only way to open those
values.

To open the data key with keys that a team or CI holds too, list them in
`encryption_keys`: age recipients that OpsTruss opens itself, and keys of
a cloud KMS that sops wraps it for:

```yaml
encryption_keys:
  - age1…
  - kms: arn:aws:kms:eu-west-1:111122223333:key/…
```

## In CI

In CI, a secret of the workflow becomes a variable, which `env:` reads:

```yaml
secrets:
  deploy_token:
    env: DEPLOY_TOKEN
```

[CI with GitHub Actions](./ci) has the rest.
