### Fixed

- **Gitea releases publish the container image.** The push now runs in its own
  `Gitea Image` job on the release runner and logs in with that runner's
  registry credential (`RELEASE_REGISTRY`, `RELEASE_REGISTRY_USER`,
  `RELEASE_REGISTRY_TOKEN`, ruling 173) instead of the workflow token, which
  the registry rejected on every release. The login lives in a private Docker
  config removed when the push ends.
- **Release notes claim an image only when it was pushed.** The Gitea release
  notes say the image was pushed only when the image job succeeded; a failed
  or skipped image job no longer blocks the binary release (#284).
