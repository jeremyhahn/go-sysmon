#!/usr/bin/env bash
#
# Fails when a git conflict marker is committed.
#
# `git add` on a conflicted file stages it WITH the markers in. For source that
# is loud -- the next build or test fails. For the committed frontend bundle it
# is silent: cmd/sysmon/frontend/dist/ is generated output that nothing compiles
# and no test reads, so a marker sits there until somebody happens to look.
#
# It has already happened twice here (#40). One rebase put "<<<<<<< HEAD" into
# dist/index.html; checking for it then turned up SIX earlier commits on the
# same branch carrying markers, accumulating 1, 2, 2, 3, 3 and 4 of them. Every
# gate passed, because later commits rebuilt dist and the branch HEAD was clean
# -- so the tree that would have been squash-merged was fine while `git show` on
# any of those six produced a broken bundle.
#
# Two scans, because those are two different failures:
#
#   tree     what would ship. This is the one that protects users.
#   history  what a bisect or a cherry-pick would land on. This is the one that
#            was contaminated while every gate was green.
#
# The history scan needs a base commit to diff against. When it cannot have one
# it SAYS SO and the scan is reported as skipped rather than passing quietly --
# a gate that reports a verdict on work it did not do is the defect this file
# exists to catch, one level up.
set -euo pipefail

# Markers are matched anchored at the start of a line, which is where git writes
# them. "=======" also has to be the WHOLE line: a row of equals signs is
# ordinary in documentation and in generated banners.
readonly MARKER_RE='^(<<<<<<< |=======$|>>>>>>> )'

# Documentation legitimately shows conflict markers when explaining them. Code
# and generated output never do.
readonly EXCLUDES=(':(exclude)*.md')

fail=0

echo "== tree: files that would be merged"
if git grep -nE "$MARKER_RE" -- . "${EXCLUDES[@]}"; then
    echo "::error::a conflict marker is present in the working tree, listed above"
    fail=1
else
    echo "ok   no conflict markers in tracked files"
fi

echo
echo "== history: each commit that would enter main"
base="${CONFLICT_SCAN_BASE:-}"
if [[ -z "$base" ]]; then
    echo "SKIPPED  no base commit given (set CONFLICT_SCAN_BASE, e.g. to the PR's base ref)."
    echo "         The tree scan above still ran; this one did not, and is not a pass."
elif ! git rev-parse --verify --quiet "$base" >/dev/null; then
    echo "SKIPPED  base commit '$base' is not in this checkout -- a shallow clone cannot"
    echo "         see it. The tree scan above still ran; this one did not, and is not a pass."
else
    commits="$(git rev-list "$base..HEAD")"
    if [[ -z "$commits" ]]; then
        echo "ok   no commits ahead of $base"
    else
        for commit in $commits; do
            if git grep -nE "$MARKER_RE" "$commit" -- . "${EXCLUDES[@]}"; then
                echo "::error::commit $commit carries a conflict marker, listed above"
                fail=1
            fi
        done
        [[ $fail -eq 0 ]] && echo "ok   $(echo "$commits" | wc -l) commit(s) clean"
    fi
fi

exit "$fail"
