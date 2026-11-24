#!/usr/bin/env bash
#
# Fails when a binary's ELF notes demand a CPU above its architecture's
# baseline.
#
# Usage: scripts/check-cpu-baseline.sh <binary>...
#
# The demand is a GNU property note. The linker merges it from every object it
# links, and glibc refuses to start a program whose note asks for more than the
# CPU has. Go writes no such note, so whatever a binary carries came from the C
# toolchain that linked it.
#
# What fails:
#
#   x86 ISA needed      anything but x86-64-baseline alone
#   x86 feature needed  anything outside x86, x87, MMX, XMM, FXSR
#   a property readelf cannot decode
#
# A binary with no such note passes. arm64 binaries are always that case: the
# AArch64 ELF ABI defines no note that demands a CPU level.
#
# Only the notes are read; no instruction is disassembled.
set -euo pipefail

# readelf translates the property names it prints.
export LC_ALL=C

readonly ISA_BASELINE='x86-64-baseline'
readonly FEATURE_BASELINE='x86|x87|MMX|XMM|FXSR'

if (($# == 0)); then
    echo "::error::no binary to check. Usage: $0 <binary>..."
    exit 1
fi
if ! command -v readelf >/dev/null 2>&1; then
    echo "::error::readelf not found: install binutils. Nothing was checked."
    exit 1
fi

# properties prints each GNU property of a binary on a line of its own.
# readelf prints the first after "Properties: " and each further one on a
# tab-indented line below it.
properties() {
    readelf --notes "$1" | awk '
        /Properties: /  { sub(/.*Properties: /, ""); print; open = 1; next }
        open && /^\t/   { sub(/^\t+/, ""); print; next }
                        { open = 0 }'
}

# failed: a file that could not be read. above: one that demands too much.
failed=0
above=0
for binary in "$@"; do
    if [[ ! -f $binary ]]; then
        echo "::error::$binary: no such file"
        failed=1
        continue
    fi
    if ! props=$(properties "$binary"); then
        echo "::error::$binary: readelf could not read its notes, for the reason it gives above"
        failed=1
        continue
    fi

    demands=()
    reasons=()
    while IFS= read -r prop; do
        case $prop in
            *'<'*)
                reasons+=("carries a property readelf cannot decode: $prop")
                ;;
            'x86 ISA needed: '*)
                demands+=("$prop")
                [[ ${prop#*: } == "$ISA_BASELINE" ]] ||
                    reasons+=("demands a CPU above the baseline: $prop")
                ;;
            'x86 feature needed: '*)
                demands+=("$prop")
                [[ ${prop#*: } =~ ^($FEATURE_BASELINE)(,\ ($FEATURE_BASELINE))*$ ]] ||
                    reasons+=("demands a CPU above the baseline: $prop")
                ;;
        esac
    done <<<"$props"

    if ((${#reasons[@]})); then
        for reason in "${reasons[@]}"; do
            echo "::error::$binary $reason"
        done
        above=1
    elif ((${#demands[@]})); then
        joined=$(printf '; %s' "${demands[@]}")
        echo "ok   $binary: ${joined#; }"
    else
        echo "ok   $binary: its notes demand no CPU level"
    fi
done

if ((above)); then
    echo "         Build it in the release builder image, never with the host's C toolchain"
    echo "         (see the build job of .github/workflows/release.yml)."
fi
exit $((failed | above))
