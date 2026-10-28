#!/bin/sh
# generate.sh <opstruss> <name> writes <name>.txtar here, which TestCompat
# runs: the project in project/, run by that opstruss, with the .opstruss/
# it leaves and the age key of its sensitive values, which is a key of
# these tests only. A version writes its file once, from its release
# binary, and the file never changes after.
set -eu

case $# in
2) ;;
*) echo "usage: $0 <opstruss> <name>" >&2; exit 2 ;;
esac
bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -r "$work"' EXIT

cp -R "$here/project/." "$work/"
cd "$work"
run() { HOME="$work/.home" OPSTRUSS_AGE_KEY_FILE="$work/key.txt" USER=fixture "$bin" "$@" >/dev/null; }
run up --yes
run remember
run recall

# What OpsTruss builds again, and the names and paths of this machine.
rm -r .home .opstruss/runs/default/.index
find .opstruss -type f \( -name '*.json' -o -name '*.jsonl' \) | while read -r f; do
	sed -e 's/"hostname": *"[^"]*"/"hostname": "fixture"/g' -e 's/"user": *"[^"]*"/"user": "fixture"/g' \
		-e "s|/private$work|.|g" -e "s|$work|.|g" "$f" >"$f.new"
	mv "$f.new" "$f"
done

version=$("$bin" version)
{
	cat <<EOF
# What $version wrote reads with this version: no unit runs again, and
# its runs, records, kv, and sensitive values read. generate.sh wrote this
# file; it never changes.

env HOME=\$WORK/home
env OPSTRUSS_AGE_KEY_FILE=\$WORK/key.txt

exec opstruss plan
stdout 'default/build: unchanged'
stdout 'default/token: unchanged'
stdout 'default/ratio: unchanged'
! stdout 'will run'
exec opstruss runs
stdout 'remember +success'
stdout 'recall +success'
exec opstruss state list
stdout 'default/build'
stdout 'token'
exec opstruss recall
stdout 'count=2 ratio=0.5 size=6'
exec opstruss logs build
stdout 'built'

EOF
	cd "$here" && go run github.com/rogpeppe/go-internal/cmd/txtar-c -a -quote "$work"
} >"$here/$2.txtar"
