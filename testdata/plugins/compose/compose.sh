#!/usr/bin/env bash
# compose.sh plan|up|down: what docker compose would do with the file of
# INPUT_FILE, for the use cases. An image resolves to its digest in
# registry.json of the working directory, or stays its tag, and INPUT_PIN
# (service → digest) wins; up writes the images it runs to
# $OPSTRUSS_OUTPUT, and every call writes a line to compose.log.
set -euo pipefail
sum() { { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1; }
project=${INPUT_PROJECT:-$(basename "$(cd "$(dirname "$INPUT_FILE")" && pwd)")}
registry='{}'
[ ! -f registry.json ] || registry=$(cat registry.json)
want=$(awk '/^services:/ {s = 1; next} /^[^ #]/ {s = 0} s && /^  [A-Za-z0-9_.-]+:/ {svc = $1; sub(":$", "", svc)} s && /^    image:/ {print svc, $2}' "$INPUT_FILE" |
  jq -Rn --argjson reg "$registry" --argjson pin "${INPUT_PIN:-"{}"}" \
    '[inputs | split(" ") | {key: .[0], value: ($pin[.[0]] // $reg[.[1]] // .[1])}] | from_entries')
have='{}'
[ -z "${OPSTRUSS_STATE:-}" ] || have=$(jq -c '.outputs.images // {}' "$OPSTRUSS_STATE")
digest=sha256:$(printf '%s' "$want" | sum)
case "$1" in
plan)
  jq -n --argjson want "$want" --argjson have "$have" --arg d "$digest" '{
    changes: ([$want | to_entries[] | select($have[.key] != .value) | {action: (if $have[.key] then "update" else "create" end), resource: .key}]
      + [$have | keys[] | select($want[.] == null) | {action: "delete", resource: .}]),
    outputs: ["images"], digest: $d, exact: true}' > "$OPSTRUSS_PLAN"
  ;;
up)
  if [ -n "${OPSTRUSS_DIGEST:-}" ] && [ "$OPSTRUSS_DIGEST" != "$digest" ]; then
    echo "compose: the images changed since the plan" >&2
    exit 1
  fi
  # the variables that the file interpolates, as compose reads them
  vars=$(grep -o '\${[A-Za-z_][A-Za-z0-9_]*' "$INPUT_FILE" | cut -c3- | sort -u || true)
  env=""
  for v in $vars; do env="$env $v=${!v:-}"; done
  echo "up $project $(jq -r 'to_entries | map("\(.key)=\(.value)") | join(" ")' <<<"$want")$env" >> compose.log
  jq -n --argjson i "$want" '{images: $i}' > "$OPSTRUSS_OUTPUT"
  ;;
down)
  echo "down $project" >> compose.log
  ;;
esac
