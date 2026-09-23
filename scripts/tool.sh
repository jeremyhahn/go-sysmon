#!/usr/bin/env bash
#
# Verifies, and on request installs, one pinned Go analysis tool.
#
#   tool.sh check  BIN PKG VERSION
#   tool.sh ensure BIN PKG VERSION GOBIN_DIR ORIGIN
#
# The pins themselves live in the Makefile (GOLANGCI_LINT_VERSION and friends),
# and only there. Local runs and every workflow go through the same targets, so
# the linter that clears a PR is the one a developer ran -- before this, the
# Makefile's install hints said @latest while ci.yml and release.yml each
# hardcoded their own versions, and nothing noticed when the three drifted.
#
# The version is read from the binary's embedded build info (`go version -m`),
# not from the tool's own --version output. Build info records the package the
# binary was built from and the module version Go resolved and checksummed, and
# it is there for every go-installed tool -- gosec installed with `go install`
# prints "Version: dev", so its self-report cannot answer the question.
#
# check  exits 0 only when BIN was built from PKG at exactly VERSION.
# ensure does nothing when check passes. Otherwise it installs PKG@VERSION into
#        GOBIN_DIR and checks that copy. On the CI runner this means a baked tool
#        at the pinned version is used as-is, and a stale image costs a download
#        rather than a gate that disagrees with the Makefile.
#        ORIGIN is make's $(origin) of the tool variable. When the caller chose
#        BIN explicitly (anything but "file"), installing into GOBIN would not
#        change what the gates run, so a wrong override is refused instead of
#        reported as fixed.
set -euo pipefail

usage() {
	echo "usage: tool.sh check BIN PKG VERSION" >&2
	echo "       tool.sh ensure BIN PKG VERSION GOBIN_DIR ORIGIN" >&2
	exit 2
}

# check prints one line saying what BIN is and returns 0 when it matches.
check() {
	local bin=$1 pkg=$2 want=$3 name info path got
	name=$(basename "$bin")
	# A bare name (ACTIONLINT=actionlint) is a command, looked up on PATH as
	# make would run it; `go version -m` would read it as a file in the cwd.
	case "$bin" in
	*/*) ;;
	*) bin=$(command -v "$bin" 2>/dev/null || echo "$bin") ;;
	esac
	# The recipe executes this file next, so metadata alone is not enough.
	if [ ! -f "$bin" ] || [ ! -x "$bin" ]; then
		echo "$name: no executable at $bin (want $pkg@$want) -- run 'make tools'" >&2
		return 1
	fi
	if ! info=$(go version -m "$bin" 2>/dev/null); then
		echo "$name: no Go binary at $bin (want $pkg@$want) -- run 'make tools'" >&2
		return 1
	fi
	path=$(awk -F'\t' '$2 == "path" { print $3; exit }' <<<"$info")
	got=$(awk -F'\t' '$2 == "mod" { print $4; exit }' <<<"$info")
	if [ "$path" = "$pkg" ] && [ "$got" = "$want" ]; then
		echo "ok   $name $want   $bin"
		return 0
	fi
	echo "$name: $bin is ${path:-unknown}@${got:-unknown}, want $pkg@$want -- run 'make tools'" >&2
	return 1
}

ensure() {
	local bin=$1 pkg=$2 want=$3 dir=$4 origin=$5
	if [ "$origin" != "file" ]; then
		# An override is what the gates will run; it has to be right as given.
		check "$bin" "$pkg" "$want" || {
			echo "$(basename "$pkg"): $bin was set explicitly ($origin); install $pkg@$want there or drop the override" >&2
			return 1
		}
		return 0
	fi
	if check "$bin" "$pkg" "$want" 2>/dev/null; then
		return 0
	fi
	echo "installing $pkg@$want into $dir"
	# go install skips a target whose build ID already matches, even one that
	# has lost its execute bit, so the rejected copy is removed first.
	rm -f "$dir/$(basename "$pkg")"
	GOBIN="$dir" go install "$pkg@$want"
	check "$dir/$(basename "$pkg")" "$pkg" "$want"
}

case "${1:-}" in
check)
	[ $# -eq 4 ] || usage
	check "$2" "$3" "$4"
	;;
ensure)
	[ $# -eq 6 ] || usage
	ensure "$2" "$3" "$4" "$5" "$6"
	;;
*)
	usage
	;;
esac
