#!/usr/bin/env bash
#
# Refuses to publish unless the release tag still names the commit the verify
# job approved.
#
# release.yml verifies one commit and publishes from later jobs. A tag is
# immutable only by convention, so a tag moved between the two would publish a
# commit that never passed the gate, and a check that the tag agrees with the
# checkout would still pass, on the new commit. So the verify job exports the
# commit it approved, and every job that publishes runs this immediately
# before it does. An absent value fails; it is never read as agreement.
#
# Required environment:
#   RELEASE_TAG   the tag being released, e.g. v0.2.0-alpha
#   RELEASE_SHA   the commit the verify job approved
#
# Run from a checkout of the repository; the tag is fetched from origin fresh.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is empty: the verify job exported no tag}"
: "${RELEASE_SHA:?RELEASE_SHA is empty: the verify job exported no commit}"

if ! git fetch --quiet --no-tags --force origin "+refs/tags/${RELEASE_TAG}:refs/tags/${RELEASE_TAG}"; then
  echo "refusing to publish: could not fetch tag ${RELEASE_TAG} from origin (deleted, or origin unreachable)" >&2
  exit 1
fi
current="$(git rev-parse --verify --quiet "refs/tags/${RELEASE_TAG}^{commit}" || true)"

if [ -z "${current}" ]; then
  echo "refusing to publish: tag ${RELEASE_TAG} does not exist on origin" >&2
  exit 1
fi
if [ "${current}" != "${RELEASE_SHA}" ]; then
  echo "refusing to publish: tag ${RELEASE_TAG} now names ${current}, but the verify job approved ${RELEASE_SHA}" >&2
  exit 1
fi
echo "tag ${RELEASE_TAG} still names the verified commit ${RELEASE_SHA}"
