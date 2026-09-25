#!/usr/bin/env bash
#
# pkgtest.sh runs one package's tests and measures its coverage, for the
# per-package make targets (test-<pkg>, coverage-<pkg>, integration-test-<pkg>).
#
#   pkgtest.sh test <package dir> <shape tags>...
#       Runs the package's unit tests, under -race, once for every distinct
#       build among the given shapes.
#
#   pkgtest.sh coverage <floor> <package dir> <shape tags>...
#       Measures the package's unit coverage over the same builds, merged, and
#       fails below <floor> percent.
#
#   pkgtest.sh integration <floor> [<package dir>...]
#       Runs the integration suite (test/integration) with every package of the
#       module instrumented -- the test binary and every sysmon binary it
#       starts -- and reports the integration coverage of the given packages,
#       or of all of them. Fails if a test fails or a package is below <floor>.
#       It runs inside the devcontainer (make integration-test), never on the
#       host. The files and packages build/coverage-exceptions names are left
#       out of the integration floor, and nothing else is.
#
#   pkgtest.sh check-exceptions
#       Validates build/coverage-exceptions: four fields per entry, a known
#       category, an owner approval, and a path that exists. Part of make lint.
#
# Each <shape tags> is a comma-separated tag list; "" is the untagged build.
# A floor of 0 reports without failing.
#
# Why once per distinct build, not once: a file behind a build tag is built,
# and so tested and measured, only in the shapes that set the tag. The union of
# every shape's run is the package's real coverage. Shapes that compile the
# package from the same files are one build, and run once.
#
# Coverage counts the package's own statements. Test files are never
# instrumented, and mock files -- a file whose name contains "mock", or any
# file under a directory named mock or mocks -- are left out (CLAUDE.md).
set -euo pipefail

MODULE=$(go list -m)
readonly MODULE

usage() {
    cat >&2 <<EOF
usage: $0 test <package dir> <shape tags>...
       $0 coverage <floor> <package dir> <shape tags>...
       $0 integration <floor> [<package dir>...]
       $0 check-exceptions
EOF
    exit 2
}

# distinct_builds prints, one per line, the first of the given tag sets for
# each distinct set of files the package is compiled and tested from. An
# untagged build prints as an empty line.
distinct_builds() {
    local dir=$1
    shift
    local -A seen=()
    local tags key
    for tags in "$@"; do
        key=$(go list -tags "$tags" -f '{{.GoFiles}}{{.CgoFiles}}{{.TestGoFiles}}{{.XTestGoFiles}}' "$dir")
        [[ -n ${seen[$key]+x} ]] && continue
        seen[$key]=1
        printf '%s\n' "$tags"
    done
}

# without_mocks drops the profile lines of mock files.
without_mocks() {
    grep -vE '(/mocks?/|/[^/:]*mock[^/:]*\.go:)' || true
}

# totals reads a text coverage profile and prints "<import path> <covered>
# <total>" per package. A block listed more than once -- one file built in two
# shapes, or run by two processes -- counts once, covered if any run covered it.
totals() {
    awk '
        /^mode:/ { next }
        {
            block = $1
            stmts[block] = $2
            if ($3 > 0) hit[block] = 1
            file = block
            sub(/:.*/, "", file)
            sub(/\/[^\/]*$/, "", file)
            pkg[block] = file
        }
        END {
            for (b in stmts) {
                total[pkg[b]] += stmts[b]
                if (b in hit) covered[pkg[b]] += stmts[b]
            }
            for (p in total) print p, covered[p] + 0, total[p]
        }'
}

# EXCEPTIONS is the file naming what the integration floor leaves out.
readonly EXCEPTIONS=build/coverage-exceptions

# exceptionCategories are the only reasons an entry may give.
declare -A exceptionCategories=([hardware]=1 [host-hardware]=1 [host]=1 [gui]=1 [defensive]=1 [test-helper]=1)

# EXEMPT_FILES and EXEMPT_PACKAGES are filled by load_exceptions: excepted Go
# files as module-relative paths, and exempt package directories without their
# trailing slash.
declare -A EXEMPT_FILES=() EXEMPT_PACKAGES=()

# trim prints its argument without leading or trailing whitespace.
trim() {
    local s=$1
    s=${s#"${s%%[![:space:]]*}"}
    printf '%s' "${s%"${s##*[![:space:]]}"}"
}

# load_exceptions reads EXCEPTIONS, failing on any entry that is malformed,
# unapproved, of an unknown category, or naming a path that does not exist --
# a stale entry would except code that is not there, and nothing would notice
# when something else took its name.
load_exceptions() {
    local line n=0 bad=0 path category reason approval extra
    while IFS= read -r line || [[ -n $line ]]; do
        n=$((n + 1))
        [[ -z ${line//[[:space:]]/} || $line == \#* ]] && continue
        IFS='|' read -r path category reason approval extra <<<"$line"
        path=$(trim "$path")
        category=$(trim "$category")
        reason=$(trim "$reason")
        approval=$(trim "$approval")
        if [[ -z $path || -z $category || -z $reason || -z $approval || -n $extra ]]; then
            echo "::error::$EXCEPTIONS:$n: want path | category | reason | approval"
            bad=1
            continue
        fi
        if [[ -z ${exceptionCategories[$category]+x} ]]; then
            echo "::error::$EXCEPTIONS:$n: unknown category \"$category\""
            bad=1
        fi
        if [[ $approval != approved* ]]; then
            echo "::error::$EXCEPTIONS:$n: no owner approval recorded"
            bad=1
        fi
        if [[ $path == */ ]]; then
            [[ -d $path ]] || { echo "::error::$EXCEPTIONS:$n: no package directory $path"; bad=1; }
            EXEMPT_PACKAGES[${path%/}]=1
        else
            [[ -f $path ]] || { echo "::error::$EXCEPTIONS:$n: no file $path"; bad=1; }
            EXEMPT_FILES[$path]=1
        fi
    done <"$EXCEPTIONS"
    return "$bad"
}

# without_exempt_files drops the profile lines of every excepted file.
without_exempt_files() {
    local patterns=() f
    for f in "${!EXEMPT_FILES[@]}"; do
        patterns+=(-e "$MODULE/$f:")
    done
    if ((${#patterns[@]} == 0)); then
        cat
        return
    fi
    grep -vF "${patterns[@]}" || true
}

# report prints one line per package directory from a merged text profile and
# fails if any package is below the floor. A package with no coverage data --
# compiled into no measured binary -- is 0%. A package EXEMPT_PACKAGES names
# is reported as exempt and never fails.
report() {
    local floor=$1 profile=$2
    shift 2
    local sums failed=0 dir line covered total pct verdict
    sums=$(without_mocks <"$profile" | without_exempt_files | totals)
    for dir in "$@"; do
        line=$(awk -v p="$MODULE/${dir#./}" '$1 == p' <<<"$sums")
        covered=0
        total=0
        if [[ -n $line ]]; then
            read -r _ covered total <<<"$line"
        fi
        pct=$(awk -v c="$covered" -v t="$total" 'BEGIN { printf "%.1f", t ? 100 * c / t : 0 }')
        verdict=ok
        if [[ -n ${EXEMPT_PACKAGES[${dir#./}]+x} ]]; then
            verdict=exempt
        elif awk -v p="$pct" -v f="$floor" 'BEGIN { exit !(p < f) }'; then
            verdict=FAIL
            failed=1
        fi
        printf '%-6s %6s%%  %s  (%d of %d statements)\n' "$verdict" "$pct" "${dir#./}" "$covered" "$total"
    done
    if ((failed)); then
        echo "::error::coverage is below the ${floor}% floor for the packages marked FAIL above."
        return 1
    fi
}

# SCRATCH is the one temporary directory a run uses, removed on exit. It is a
# global, not a function's local, because the exit trap runs after every
# function has returned.
SCRATCH=
cleanup() {
    if [[ -n $SCRATCH ]]; then
        rm -rf "$SCRATCH"
    fi
}
trap cleanup EXIT

# textfmt merges Go binary coverage data directories into a text profile.
textfmt() {
    local out=$1
    shift
    local IFS=,
    go tool covdata textfmt -i "$*" -o "$out"
}

cmd_test() {
    (($# >= 2)) || usage
    local dir=$1 tags
    shift
    while IFS= read -r tags; do
        echo "test: $dir, tags \"$tags\""
        go test -tags "$tags" -race -count=1 -v "$dir"
    done < <(distinct_builds "$dir" "$@")
}

cmd_coverage() {
    (($# >= 3)) || usage
    local floor=$1 dir=$2 tags
    shift 2
    SCRATCH=$(mktemp -d)
    local n=0 work=$SCRATCH
    local runs=()
    while IFS= read -r tags; do
        n=$((n + 1))
        mkdir -p "$work/$n"
        echo "coverage: $dir, tags \"$tags\""
        go test -tags "$tags" -count=1 -cover "$dir" -args -test.gocoverdir="$work/$n"
        runs+=("$work/$n")
    done < <(distinct_builds "$dir" "$@")
    textfmt "$work/profile.txt" "${runs[@]}"
    report "$floor" "$work/profile.txt" "$dir"
}

cmd_integration() {
    (($# >= 1)) || usage
    local floor=$1
    shift
    local dirs=("$@")
    if ((${#dirs[@]} == 0)); then
        mapfile -t dirs < <(go list -f '{{.Dir}}' ./... | sed "s|^$PWD/|./|")
    fi
    SCRATCH=$(mktemp -d)
    local work=$SCRATCH
    # SYSMON_COVERDIR reaches every sysmon binary the suite starts, which
    # TestMain builds with -cover when it is set; -test.gocoverdir is the test
    # binary's own. Both write into one directory, merged below.
    #
    # INTEGRATION_SUITES narrows the run to some of the suites under
    # test/integration, for working on one; the gate runs them all.
    # INTEGRATION_PROFILE, when set, is where the merged text profile is copied,
    # for reading coverage file by file.
    local status=0
    # shellcheck disable=SC2086 # INTEGRATION_SUITES is a list of packages.
    SYSMON_COVERDIR="$work" go test -v -tags integration -count=1 -timeout 900s \
        -cover -coverpkg="$MODULE/..." ${INTEGRATION_SUITES:-./test/integration/...} \
        -args -test.gocoverdir="$work" || status=$?
    textfmt "$work/profile.txt" "$work"
    if [[ -n ${INTEGRATION_PROFILE:-} ]]; then
        cp "$work/profile.txt" "$INTEGRATION_PROFILE"
    fi
    load_exceptions || status=1
    echo "integration coverage (excepting $EXCEPTIONS):"
    report "$floor" "$work/profile.txt" "${dirs[@]}" || status=1
    return "$status"
}

cmd_check_exceptions() {
    (($# == 0)) || usage
    load_exceptions
    echo "ok   $EXCEPTIONS: ${#EXEMPT_FILES[@]} files and ${#EXEMPT_PACKAGES[@]} packages, each approved and present"
}

(($# >= 1)) || usage
declare -A commands=([test]=cmd_test [coverage]=cmd_coverage [integration]=cmd_integration [check-exceptions]=cmd_check_exceptions)
command=$1
shift
[[ -n ${commands[$command]+x} ]] || usage
"${commands[$command]}" "$@"
