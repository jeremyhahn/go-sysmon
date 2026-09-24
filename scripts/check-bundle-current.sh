#!/usr/bin/env bash
# Verify the committed dashboard bundle matches the source it was built from.
#
# cmd/sysmon/frontend/dist is generated output that is COMMITTED on purpose:
# //go:embed all:frontend/dist means a clone without it cannot `go build`, so the
# Go toolchain alone has to be enough to build this project. The cost of that
# choice is a second copy of the truth, and nothing checked the two agreed.
#
# They have disagreed three times. Most recently `main` carried #130's
# "PSI not available on this host" card and #132's swap types in src/ while
# serving a bundle built before either -- every test passing, because no test
# reads the bundle.
#
# Built to a scratch directory and compared, never in place: a check that
# rebuilds the committed copy destroys the evidence it was meant to report, and
# would leave a dirty tree behind on a machine where the answer was "yes".
set -euo pipefail

FRONTEND_DIR="${FRONTEND_DIR:-cmd/sysmon/frontend}"
DIST_PATH="$FRONTEND_DIR/dist"

# No check that $DIST_PATH exists in the working tree: what is compared comes from
# HEAD, so requiring the worktree copy could only produce false failures -- after
# `make clean`, or while a concurrent build has it momentarily empty. The
# extraction below is the real precondition, and it is checked there.
if [ ! -x "$FRONTEND_DIR/node_modules/.bin/vite" ]; then
	echo "vite is not installed in $FRONTEND_DIR." >&2
	echo "This check needs the frontend toolchain. Run: make frontend-deps" >&2
	exit 1
fi

fresh="$(mktemp -d)"
committed="$(mktemp -d)"
trap 'rm -rf "$fresh" "$committed"' EXIT

# Compared against what is COMMITTED at HEAD, not against the working tree's
# dist/, and that is the whole reason this is race-free.
#
# `make build` runs build-frontend, which overwrites dist/ in place. A check that
# read the working tree could therefore be handed a bundle regenerated moments
# earlier and call a stale commit current -- which meant the check's correctness
# depended on running before any build, which in turn meant the aggregate targets
# had to be serialized. Reading HEAD removes that coupling: no order, no -j
# hazard, and the question asked is the accurate one. The committed bundle is what
# //go:embed compiles into the binary; the working tree's copy is not.
if ! git -C . archive HEAD "$DIST_PATH" 2>/dev/null | tar -x -C "$committed" 2>/dev/null; then
	echo "could not read $DIST_PATH from HEAD; is this a git checkout?" >&2
	exit 1
fi
COMMITTED="$committed/$DIST_PATH"
[ -d "$COMMITTED" ] || { echo "HEAD carries no $DIST_PATH" >&2; exit 1; }

if ! ( cd "$FRONTEND_DIR" && ./node_modules/.bin/vite build --outDir "$fresh" --emptyOutDir ) >"$fresh.log" 2>&1; then
	echo "the frontend build failed, so the bundle could not be checked:" >&2
	tail -20 "$fresh.log" >&2
	rm -f "$fresh.log"
	exit 1
fi
rm -f "$fresh.log"

if diff -r -q "$fresh" "$COMMITTED" >/dev/null 2>&1; then
	echo "bundle: committed output matches its source"
	exit 0
fi

echo "THE COMMITTED BUNDLE DOES NOT MATCH ITS SOURCE." >&2
echo >&2
# `|| true`, because diff exits 1 when the trees differ -- which is the case we
# are in. Under `set -e -o pipefail` this pipeline would otherwise end the script
# right here, and every one of the explanatory lines below would be unreachable
# for the only failure they exist to explain.
diff -r -q "$fresh" "$COMMITTED" 2>&1 | sed 's/^/  /' >&2 || true
echo >&2
echo "The bundle committed at HEAD is not the one src/ compiles to, so the binary" >&2
echo "embeds a dashboard older than its own source. Rebuild it and commit it in the" >&2
echo "same change that touched the source:" >&2
echo >&2
# $DIST_PATH, not $COMMITTED: the latter now points into the throwaway extraction
# of HEAD, and telling someone to `git add` a temp directory is worse than saying
# nothing.
echo "    make build-frontend && git add $DIST_PATH" >&2
exit 1
