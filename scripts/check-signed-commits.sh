#!/usr/bin/env bash
#
# Fails when a commit on this branch is unsigned.
#
# Signed commits are ratified for every agent and enforced on main -- but every
# merge is a Gitea SQUASH, a new commit Gitea signs itself, so the enforcement
# on main never sees a branch's commits at all. Nothing checked them, and the
# guarantee was decorative on the one path that could break it (#41).
#
# The path that breaks it: THE SIGNING DECISION IS FIXED WHEN A REBASE STARTS.
# A rebase begun with plain `git rebase` recreates every commit unsigned, and
# `agent-git.sh rebase --continue` on it reports success. agent-git.sh says so
# in its own header, which did not stop it happening on eight commits of one
# branch -- or on this repository's own #82, where a plain rebase produced an
# unsigned commit that was only re-signed because the author happened to amend
# it through the wrapper afterwards for an unrelated reason.
#
# `git cat-file` is used rather than `--format=%G?`, and that is not a
# preference. %G? prints N for BOTH "unsigned" and "signed but unverifiable
# without gpg.ssh.allowedSignersFile" -- which no agent has -- so it reports N
# on correctly signed commits and cannot tell the two apart. A check built on it
# would fail everything and be disabled within a day. The presence of a gpgsig
# header is the question being asked here; whether the signature verifies is
# branch protection's job, and it has the allowed-signers file to do it.
set -euo pipefail

base="${SIGNED_SCAN_BASE:-}"

if [[ -z "$base" ]]; then
    echo "SKIPPED  no base commit given (set SIGNED_SCAN_BASE, e.g. to the PR's base ref)."
    echo "         Nothing was checked. This is not a pass."
    exit 0
fi
if ! git rev-parse --verify --quiet "$base" >/dev/null; then
    echo "SKIPPED  base commit '$base' is not in this checkout -- a shallow clone cannot"
    echo "         see it. Nothing was checked. This is not a pass."
    exit 0
fi

commits="$(git rev-list "$base..HEAD")"
if [[ -z "$commits" ]]; then
    echo "ok   no commits ahead of $base"
    exit 0
fi

unsigned=()
for commit in $commits; do
    if ! git cat-file commit "$commit" | grep -q '^gpgsig'; then
        unsigned+=("$commit")
    fi
done

if [[ ${#unsigned[@]} -eq 0 ]]; then
    echo "ok   $(echo "$commits" | wc -l) commit(s) signed"
    exit 0
fi

for commit in "${unsigned[@]}"; do
    echo "::error::unsigned commit $commit  $(git log -1 --format=%s "$commit")"
done
cat <<'MSG'

Every commit needs a signature. The usual cause is a rebase started with plain
`git rebase`: the signing decision is fixed at that moment, so every commit it
recreates is unsigned and `agent-git.sh rebase --continue` reports success.

To fix, redo the rebase THROUGH the wrapper from the start:

    software/scripts/agent-git.sh <repo> rebase <base>

Re-running it that way signs every recreated commit.
MSG
exit 1
