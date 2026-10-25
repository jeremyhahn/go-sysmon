#!/usr/bin/env bash
#
# Publishes a release to a Gitea instance: creates or updates the release for
# the tag, then uploads everything in dist/ as assets.
#
# This lives in a script rather than inline in the workflow because the logic
# needs multi-line JSON handling, and embedding that in a YAML block scalar is
# how you get a workflow that fails to parse.
#
# Gitea's release API is GitHub-shaped, so plain curl is enough and there is no
# dependency on a third-party action being fetchable from the runner.
#
# Required environment:
#   GITEA_API    e.g. http://git.example.internal/api/v1
#   GITEA_REPO   owner/name
#   GITEA_TOKEN  token with write access to the repository
#   RELEASE_TAG  e.g. v0.1.0
set -euo pipefail

: "${GITEA_API:?}"; : "${GITEA_REPO:?}"; : "${GITEA_TOKEN:?}"; : "${RELEASE_TAG:?}"
AUTH="Authorization: token ${GITEA_TOKEN}"
BASE="${GITEA_API}/repos/${GITEA_REPO}/releases"

json_get() { python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(d, dict):
    v = d.get(sys.argv[1], "")
    print(v if v is not None else "")' "$1"; }

# A tag with a pre-release suffix (-alpha or -rc.N, release-versioning.md §1a;
# check-changelog has already refused every other shape) is published as a
# pre-release, so it is never offered as the latest stable release. Set on
# create AND on a re-run: a release made before this rule, or edited by hand,
# is corrected rather than left marked stable.
prerelease=false
case "${RELEASE_TAG}" in *-*) prerelease=true ;; esac

# Re-releasing a tag should update it, not fail.
release_id="$(curl -sS -H "${AUTH}" "${BASE}/tags/${RELEASE_TAG}" | json_get id)"

if [ -n "${release_id}" ]; then
  echo "release ${RELEASE_TAG} already exists (id ${release_id}); replacing assets"
  updated="$(printf '{"prerelease": %s}' "${prerelease}" \
    | curl -sS -X PATCH -H "${AUTH}" -H 'Content-Type: application/json' \
        --data-binary @- "${BASE}/${release_id}" | json_get prerelease)"
  if [ "${updated}" != "$([ "${prerelease}" = true ] && echo True || echo False)" ]; then
    echo "failed to set prerelease=${prerelease} on release ${RELEASE_TAG}" >&2
    exit 1
  fi
else
  body="Automated release from Gitea Actions.

Binaries are attached below. The container image for this version was pushed to
this instance's registry."
  payload="$(python3 -c 'import json,sys; print(json.dumps({
    "tag_name": sys.argv[1],
    "name": "go-sysmon " + sys.argv[1],
    "body": sys.argv[2],
    "draft": False,
    "prerelease": sys.argv[3] == "true",
}))' "${RELEASE_TAG}" "${body}" "${prerelease}")"

  release_id="$(printf '%s' "${payload}" \
    | curl -sS -X POST -H "${AUTH}" -H 'Content-Type: application/json' \
        --data-binary @- "${BASE}" | json_get id)"

  if [ -z "${release_id}" ]; then
    echo "failed to create the release" >&2
    exit 1
  fi
  echo "created release ${RELEASE_TAG} (id ${release_id})"
fi

# Existing asset names, so a re-run replaces rather than accumulates copies.
curl -sS -H "${AUTH}" "${BASE}/${release_id}/assets" | python3 -c 'import json,sys
try:
    assets = json.load(sys.stdin) or []
except Exception:
    assets = []
for a in assets:
    print(a.get("id"), a.get("name"))' > /tmp/gitea-assets.txt

failed=0
for f in dist/*; do
  [ -f "$f" ] || continue
  name="$(basename "$f")"

  old="$(awk -v n="${name}" '$2 == n {print $1}' /tmp/gitea-assets.txt)"
  if [ -n "${old}" ]; then
    curl -sS -X DELETE -H "${AUTH}" "${BASE}/${release_id}/assets/${old}" >/dev/null
  fi

  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "${AUTH}" \
    -F "attachment=@${f}" "${BASE}/${release_id}/assets?name=${name}")"

  printf '  %-46s http %s\n' "${name}" "${code}"
  case "${code}" in
    2*) ;;
    *) failed=1 ;;
  esac
done

if [ "${failed}" -ne 0 ]; then
  echo "one or more assets failed to upload" >&2
  exit 1
fi
count=$(find dist -maxdepth 1 -type f | wc -l)
echo "release ${RELEASE_TAG} published with ${count} assets"
