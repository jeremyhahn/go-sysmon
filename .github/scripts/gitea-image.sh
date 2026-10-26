#!/usr/bin/env bash
#
# Builds and pushes the container image to the Gitea registry.
#
# Rulings 169 and 173: the push runs on the release runner alone, and it reads
# that runner's credential contract and nothing else. The release runner's env
# file exports these three names into the job environment:
#   RELEASE_REGISTRY        the registry's host[:port]; the image is pushed there
#   RELEASE_REGISTRY_USER   the login's user
#   RELEASE_REGISTRY_TOKEN  the login's password, fed to docker login on stdin
#
# Also required: VERSION, GITHUB_REPOSITORY, and RELEASE_TAG and RELEASE_SHA
# (the tag and the commit release.yml's verify job approved). No value is ever
# printed.
set -euo pipefail

# Name the missing variable, never a value.
require() {
  local name
  for name in "$@"; do
    if [ -z "${!name:-}" ]; then
      echo "gitea-image.sh: ${name} is empty or unset" >&2
      exit 1
    fi
  done
}

require RELEASE_REGISTRY RELEASE_REGISTRY_USER RELEASE_REGISTRY_TOKEN
require VERSION GITHUB_REPOSITORY RELEASE_TAG RELEASE_SHA

# The login lives in a private config directory removed on exit, so the
# credential outlives neither this script nor reaches another job on the same
# Docker host.
DOCKER_CONFIG="$(mktemp -d)"
export DOCKER_CONFIG
trap 'rm -rf "${DOCKER_CONFIG}"' EXIT

# Registry paths must be lowercase.
repo="$(printf '%s' "${GITHUB_REPOSITORY}" | tr '[:upper:]' '[:lower:]')"
image="${RELEASE_REGISTRY}/${repo}"

printf '%s' "${RELEASE_REGISTRY_TOKEN}" \
  | docker login "${RELEASE_REGISTRY}" --username "${RELEASE_REGISTRY_USER}" --password-stdin

# :latest is the stable image. A pre-release (-alpha or -rc.N: any tag with a
# hyphen; check-changelog has already refused every other shape) never moves it.
tags=(-t "${image}:${VERSION}")
case "${RELEASE_TAG}" in
  *-*) ;;
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
  *-*) echo "pushed ${image}:${VERSION} (pre-release: :latest left alone)" ;;
  *) docker push "${image}:latest"; echo "pushed ${image}:${VERSION} and :latest" ;;
esac
