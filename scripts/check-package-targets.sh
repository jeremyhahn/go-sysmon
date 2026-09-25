#!/usr/bin/env bash
#
# Fails when a Go package is missing one of its make targets.
#
# Every package has test-<name>, coverage-<name> and integration-test-<name>,
# and bench-<name> when it has a benchmark (CLAUDE.md, #167). The Makefile
# generates them from `go list`, so this guards the generation itself: a
# package the list misses, a benchmark the Makefile does not see, or two
# directories mapping to one <name>, where the second's targets would silently
# run the first's package.
#
# <name> is the directory with a leading pkg/ or internal/ dropped and each
# remaining slash a dash, the rule the Makefile's pkg_name applies.
set -euo pipefail

# Every target make knows, from one read of its database rather than one make
# run per target.
declare -A known=()
while IFS= read -r target; do
    known[$target]=1
done < <(make -pRrq -f Makefile : 2>/dev/null | sed -nE 's/^([A-Za-z0-9_.%-]+):([^=]|$).*/\1/p')

declare -A owner=()
failed=0

while IFS= read -r dir; do
    name=${dir#pkg/}
    name=${name#internal/}
    name=${name//\//-}
    if [[ -n ${owner[$name]+x} ]]; then
        echo "::error::$dir and ${owner[$name]} both map to the target name $name"
        failed=1
        continue
    fi
    owner[$name]=$dir

    targets=(test-"$name" coverage-"$name" integration-test-"$name")
    if grep -qsE '^func Benchmark' "$dir"/*_test.go; then
        targets+=(bench-"$name")
    fi
    for target in "${targets[@]}"; do
        if [[ -z ${known[$target]+x} ]]; then
            echo "::error::$dir has no make target $target"
            failed=1
        fi
    done
done < <(go list -f '{{.Dir}}' ./... | sed "s|^$PWD/||")

if ((failed)); then
    exit 1
fi
echo "ok   every package has its test-, coverage-, integration-test- and bench- targets"
