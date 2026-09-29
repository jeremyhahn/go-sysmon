#!/usr/bin/env bash
#
# Builds and pushes the container image to a Gitea instance's registry.
#
# Required environment: REGISTRY (host:port), VERSION, GITEA_TOKEN,
# GITHUB_REPOSITORY, GITHUB_ACTOR, and RELEASE_TAG and RELEASE_SHA (the tag and
# the commit release.yml's verify job approved).
set -euo pipefail

: "${REGISTRY:?}"; : "${VERSION:?}"; : "${GITEA_TOKEN:?}"
: "${GITHUB_REPOSITORY:?}"; : "${GITHUB_ACTOR:?}"
: "${RELEASE_TAG:?}"; : "${RELEASE_SHA:?}"

# Registry paths must be lowercase.
repo="$(printf '%s' "${GITHUB_REPOSITORY}" | tr '[:upper:]' '[:lower:]')"
image="${REGISTRY}/${repo}"

printf '%s' "${GITEA_TOKEN}" \
  | docker login "${REGISTRY}" --username "${GITHUB_ACTOR}" --password-stdin

# :latest is the stable image. An -alpha release is a pre-release and never
# moves it.
tags=(-t "${image}:${VERSION}")
case "${RELEASE_TAG}" in
  *-alpha) ;;
  *) tags+=(-t "${image}:latest") ;;
esac

docker build \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "GIT_COMMIT=${RELEASE_SHA}" \
  "${tags[@]}" \
  .

# The build takes minutes. Confirm the tag still names the verified commit
# after it, immediately before anything is pushed, not only before it started.
bash "$(dirname "$0")/confirm-release-ref.sh"

docker push "${image}:${VERSION}"
case "${RELEASE_TAG}" in
  *-alpha) echo "pushed ${image}:${VERSION} (pre-release: :latest left alone)" ;;
  *) docker push "${image}:latest"; echo "pushed ${image}:${VERSION} and :latest" ;;
esac
