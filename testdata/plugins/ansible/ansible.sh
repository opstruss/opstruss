#!/usr/bin/env bash
# ansible.sh: what ansible-playbook would do, for the use cases: a line to
# ansible.log, and the set_stats data of stats.json beside the playbook as
# the outputs.
set -euo pipefail
echo "playbook $INPUT_PLAYBOOK ${INPUT_INVENTORY:-}" >> ansible.log
cat "$(dirname "$INPUT_PLAYBOOK")/stats.json" > "$OPSTRUSS_OUTPUT"
