#!/bin/sh
# Prepares the tool for opstruss exec, then runs the program it was given.
echo "faketool state: $(cat "$OPSTRUSS_STATE_DIR/id" 2>/dev/null || echo none)"
exec "$@"
