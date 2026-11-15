#!/bin/sh
# Installs OpsTruss from a release on GitHub:
#
#   curl -fsSL https://opstruss.github.io/install.sh | sh
#
# OPSTRUSS_VERSION picks a release, such as 1.0.0, instead of the latest,
# and OPSTRUSS_INSTALL_DIR the directory, instead of ~/.local/bin.

set -eu

repo=https://github.com/opstruss/opstruss
docs=https://opstruss.github.io/

# color decides as opstruss does: NO_COLOR, FORCE_COLOR, and CLICOLOR_FORCE
# first, then none with TERM=dumb, then at a terminal and in GitHub Actions.
color() {
	[ -z "${NO_COLOR:-}" ] || return 1
	for v in "${FORCE_COLOR:-}" "${CLICOLOR_FORCE:-}"; do
		case $v in '' | 0) ;; *) return 0 ;; esac
	done
	[ "${TERM:-}" != dumb ] || return 1
	[ -t 2 ] || [ "${GITHUB_ACTIONS:-}" = true ]
}

if color; then
	esc=$(printf '\033')
	bold="${esc}[1m" red="${esc}[31m" green="${esc}[32m" cyan="${esc}[36m" reset="${esc}[0m"
else
	bold='' red='' green='' cyan='' reset=''
fi

start() {
	printf '%s▸%s %-10s %s\n' "$cyan" "$reset" "$1" "$2" >&2
}

ok() {
	printf '%s✓%s %-10s %s\n' "$green" "$reset" "$1" "$2" >&2
}

fail() {
	printf '%s✗%s %s\n' "$red" "$reset" "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "needs $1"
}

# get downloads $1 to $2, with the curl option $3 for progress, and fails
# when the server has no such file. When curl itself fails, it says why.
get() {
	code=$(curl --proto '=https' --tlsv1.2 -L "$3" -o "$2" -w '%{http_code}' "$1") || fail "could not download $1"
	[ "$code" = 200 ]
}

# tilde shows a path under HOME with ~, and home with $HOME, as a profile
# line that works for any user.
tilde() {
	case $1 in
	"${HOME:-/nonexistent}"/*) printf '%s/%s' '~' "${1#"$HOME"/}" ;;
	*) printf %s "$1" ;;
	esac
}

home() {
	case $1 in
	"${HOME:-/nonexistent}"/*) printf "\$HOME/%s" "${1#"$HOME"/}" ;;
	*) printf %s "$1" ;;
	esac
}

main() {
	printf '%sOpsTruss install%s\n' "$bold" "$reset" >&2
	need curl
	need tar
	need uname
	if command -v sha256sum >/dev/null 2>&1; then
		sha256() { sha256sum -c "$1"; }
	elif command -v shasum >/dev/null 2>&1; then
		sha256() { shasum -a 256 -c "$1"; }
	else
		fail 'needs sha256sum or shasum'
	fi

	os=$(uname -s)
	case $os in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	MINGW* | MSYS* | CYGWIN*) fail 'runs on Windows in WSL2: run this in a shell of WSL2' ;;
	*) fail "has no build for $os" ;;
	esac
	arch=$(uname -m)
	case $arch in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "has no build for $arch" ;;
	esac

	version=${OPSTRUSS_VERSION:-}
	which=''
	if [ -z "$version" ]; then
		# The latest release redirects to its tag without the API and its
		# rate limit; with only release candidates it redirects to the list.
		latest=$(curl --proto '=https' --tlsv1.2 -sS -o /dev/null -w '%{redirect_url}' "$repo/releases/latest") ||
			fail 'could not find the latest release'
		case $latest in
		"$repo"/releases/tag/v*) version=${latest#"$repo"/releases/tag/} which=', the latest' ;;
		*) fail 'GitHub has no latest release yet; set OPSTRUSS_VERSION, such as 1.0.0-rc.1' ;;
		esac
	fi
	version=${version#v}
	case $version in
	'' | *[!0-9A-Za-z.+-]*) fail "OPSTRUSS_VERSION is not a version: $version" ;;
	esac
	start release "v$version$which, for $os/$arch"

	archive=opstruss_${version}_${os}_${arch}.tar.gz
	tag=$repo/releases/download/v$version
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	start download checksums.txt
	get "$tag/checksums.txt" "$tmp/checksums.txt" -sS || fail "found no release v$version"
	awk -v f="$archive" '$2 == f' "$tmp/checksums.txt" >"$tmp/want"
	[ -s "$tmp/want" ] || fail "checksums.txt has no line for $archive"
	start download "$archive"
	bar=-sS
	[ ! -t 2 ] || bar=--progress-bar
	get "$tag/$archive" "$tmp/$archive" "$bar" || fail "found no $archive in release v$version"
	(cd "$tmp" && sha256 want >/dev/null) || fail "$archive does not match checksums.txt"
	ok verified "sha256 of $archive matches checksums.txt"
	tar -xzf "$tmp/$archive" -C "$tmp" opstruss

	dir=${OPSTRUSS_INSTALL_DIR:-$HOME/.local/bin}
	mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ] ||
		fail "cannot write to $(tilde "$dir"); set OPSTRUSS_INSTALL_DIR to another directory"
	# A daemon may run the old binary: mv in the same directory replaces it
	# in one step.
	cp "$tmp/opstruss" "$dir/.opstruss.$$"
	chmod 755 "$dir/.opstruss.$$"
	mv -f "$dir/.opstruss.$$" "$dir/opstruss"
	runs=$("$dir/opstruss" version) || fail "$(tilde "$dir/opstruss") does not run"
	ok installed "$(tilde "$dir/opstruss"), $runs"

	case :$PATH: in
	*:"$dir":*) ;;
	*)
		case ${SHELL:-} in
		*/fish) hint="run this once in fish:" line="fish_add_path $(home "$dir")" ;;
		*/zsh) hint="add this line to ~/.zshrc, then open a new shell:" ;;
		*/bash) hint="add this line to ~/.bashrc, then open a new shell:" ;;
		*) hint="add this line to the profile of your shell:" ;;
		esac
		printf '\n%s is not in PATH; %s\n\n  %s%s%s\n' "$(tilde "$dir")" "$hint" "$bold" "${line:-export PATH=\"$(home "$dir"):\$PATH\"}" "$reset" >&2
		;;
	esac
	printf '\nGet started at %s\n' "$docs" >&2
}

# The script runs only from this line, so a download that stops halfway
# runs nothing.
main
