#!/bin/sh
# exec.sh <program> [args]: the program with the tool's state, as tofu
# finds the state of its working directory.
export FAKE_TOFU_STATE="$OPSTRUSS_STATE_DIR/terraform.tfstate"
exec "$@"
