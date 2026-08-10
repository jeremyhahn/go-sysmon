#!/usr/bin/env bash
#
# Builds and pushes the container image to a Gitea instance's registry.
#
# Required environment: REGISTRY (host:port), VERSION, GITEA_TOKEN,
# GITHUB_REPOSITORY, GITHUB_ACTOR, GITHUB_SHA.
set -euo pipefail

: "${REGISTRY:?}"; : "${VERSION:?}"; : "${GITEA_TOKEN:?}"
: "${GITHUB_REPOSITORY:?}"; : "${GITHUB_ACTOR:?}"

# Registry paths must be lowercase.
repo="$(printf '%s' "${GITHUB_REPOSITORY}" | tr '[:upper:]' '[:lower:]')"
image="${REGISTRY}/${repo}"

printf '%s' "${GITEA_TOKEN}" \
  | docker login "${REGISTRY}" --username "${GITHUB_ACTOR}" --password-stdin

docker build \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "GIT_COMMIT=${GITHUB_SHA:-unknown}" \
  -t "${image}:${VERSION}" \
  -t "${image}:latest" \
  .

docker push "${image}:${VERSION}"
docker push "${image}:latest"
echo "pushed ${image}:${VERSION} and :latest"
