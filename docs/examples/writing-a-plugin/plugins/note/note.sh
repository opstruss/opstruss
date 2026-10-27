#!/bin/sh
# A text file as a unit: plan compares the file with the text, up writes
# it, and destroy removes it. Each program starts in the directory of the
# file of the unit, with the inputs as INPUT_PATH and INPUT_TEXT.
set -eu

case "$1" in
plan)
	if [ ! -f "$INPUT_PATH" ]; then
		action=create
	elif [ "$(cat "$INPUT_PATH")" != "$INPUT_TEXT" ]; then
		action=update
	else
		echo '{"changes": [], "exact": false}' >"$OPSTRUSS_PLAN"
		exit 0
	fi
	printf '{"changes": [{"action": "%s", "resource": "%s"}], "outputs": ["path"], "exact": false}\n' \
		"$action" "$INPUT_PATH" >"$OPSTRUSS_PLAN"
	;;
up)
	mkdir -p "$(dirname "$INPUT_PATH")"
	printf '%s\n' "$INPUT_TEXT" >"$INPUT_PATH"
	echo "path=$INPUT_PATH" >>"$OPSTRUSS_OUTPUT"
	;;
destroy)
	rm -f "$INPUT_PATH"
	;;
esac
