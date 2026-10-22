#!/usr/bin/env bash
# tofu.sh plan|up|down: what tofu would do with the .tf files of
# INPUT_SOURCE, for the use cases. The resources are the resource blocks of
# those files, up computes the outputs with outputs.jq of INPUT_SOURCE from
# $vars, and the tool's own state, terraform.tfstate in
# $OPSTRUSS_STATE_DIR, keeps vars, resources, and outputs. Every up and
# down writes a line to tofu.log with the names of the credentials it got.
set -euo pipefail
sum() { { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1; }
src=$INPUT_SOURCE
tfstate=$OPSTRUSS_STATE_DIR/terraform.tfstate
resources=$(cat "$src"/*.tf | sed -n 's/^resource "\([^"]*\)" "\([^"]*\)".*/\1.\2/p' | jq -Rn '[inputs]')
want=$(jq -n --argjson v "${INPUT_VARS:-"{}"}" --argjson r "$resources" --arg h "$(cat "$src"/*.tf | sum)" '{vars: $v, resources: $r, hash: $h}')
have=null
[ ! -f "$tfstate" ] || have=$(jq -c '{vars, resources, hash}' "$tfstate")
digest=sha256:$(printf '%s' "$want" | sum)
creds=$(env | grep -oE '^([A-Z][A-Z0-9_]*_TOKEN|AWS_[A-Z_]+|TF_VAR_[A-Za-z0-9_]+)=' | tr -d = | sort | tr '\n' ' ' || true)
case "$1" in
plan)
  jq -n --argjson want "$want" --argjson have "$have" --arg d "$digest" '
    (if $have == null then "create" elif $have == $want then null else "update" end) as $a |
    {changes: (if $a then [$want.resources[] | {action: $a, resource: .}] else [] end), digest: $d, exact: true}' > "$OPSTRUSS_PLAN"
  ;;
up)
  if [ -n "${OPSTRUSS_DIGEST:-}" ] && [ "$OPSTRUSS_DIGEST" != "$digest" ]; then
    echo "tofu: the plan is stale" >&2
    exit 1
  fi
  outputs='{}'
  [ ! -f "$src/outputs.jq" ] || outputs=$(jq -n --argjson vars "${INPUT_VARS:-"{}"}" -f "$src/outputs.jq")
  jq -n --argjson w "$want" --argjson o "$outputs" '$w + {outputs: $o}' > "$tfstate"
  echo "apply $src ${creds% }" >> tofu.log
  printf '%s' "$outputs" > "$OPSTRUSS_OUTPUT"
  ;;
down)
  rm -f "$tfstate"
  echo "destroy $src ${creds% }" >> tofu.log
  ;;
esac
