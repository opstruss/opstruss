#!/bin/sh
# generate.sh <opstruss> <name> writes <name>.txtar here, which TestCompat
# runs: the project in project/, run by that opstruss, with the .opstruss/
# it leaves and the age key of its sensitive values, which is a key of
# these tests only. A version writes its file once, from its release
# binary, and the file never changes after, so its checks read only what
# stays: --json, exit codes, and the files that the project writes, never
# the text that OpsTruss prints for people.
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

# Two runs come before changing.yaml is as project/ has it, each with
# one of its own: the first makes old and gone, and the second forgets
# old and leaves gone an orphan. flaky fails in both, as ready is
# missing, with a destroy that changes between them, so its record keeps
# a pending attempt and an abandoned one; that failure is why up exits
# with 1.
mv changing.yaml last.yaml
cat >changing.yaml <<'EOF'
units:
  flaky:
    run: test -f ready
    destroy: echo flaky-a >> destroyed
  old:
    run: echo old
  gone:
    run: echo gone
    destroy: echo gone >> destroyed
EOF
run up --yes || true
run remember
run recall
cat >changing.yaml <<'EOF'
units:
  flaky:
    run: test -f ready
    destroy: echo flaky-b >> destroyed
removed:
  - {from: old, destroy: false}
EOF
run up --yes || true
mv last.yaml changing.yaml

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
# What $version wrote reads with this version: no unit that finished runs
# again, and its runs, records, kv, and sensitive values read. up runs the
# attempt it left pending, then destroys the attempt it abandoned and its
# orphan, and destroy takes away the units of its plugin and component,
# each by the destroy that its record keeps. generate.sh wrote this file;
# it never changes.

env HOME=\$WORK/home
env OPSTRUSS_AGE_KEY_FILE=\$WORK/key.txt

exec opstruss plan
exec opstruss plan --json
stdout '"reason":\s*"pending"'
stdout '"reason":\s*"removed"'
exec opstruss runs --json
stdout '"task":\s*"remember"'
stdout '"task":\s*"recall"'
exec opstruss state list --json
stdout '"address":\s*"default/build"'
stdout '"key":\s*"_kv/token"'
stdout '"pending":\s*"up"'
stdout '"abandoned":\s*1'
stdout '"orphan":\s*true'
stdout '"forgotten":\s*true'
exec opstruss recall
stdout 'count=2 ratio=0.5 size=6'
exec opstruss logs build
stdout 'built'

exec touch ready
exec opstruss up --yes
grep -count=1 '^build$' ran
grep -count=1 '^token$' ran
grep -count=1 '^ratio$' ran
grep -count=1 '^note$' ran
grep -count=1 '^inner$' components/pair/ran
grep '^flaky-a$' destroyed
grep '^gone$' destroyed
! grep 'flaky-b|note|inner' destroyed
exec opstruss destroy --yes
grep '^flaky-b$' destroyed
grep '^note hello$' destroyed
grep '^inner$' components/pair/destroyed

EOF
	cd "$here" && go run github.com/rogpeppe/go-internal/cmd/txtar-c -a -quote "$work"
} >"$here/$2.txtar"
