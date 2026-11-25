### Changed

- **The devcontainer's base image is pinned by digest.**
  `build/docker/devcontainer/Dockerfile` named `golang:1.27.4-trixie` by tag
  alone, so the integration suite ran on whatever that tag pointed at on the
  day. It now carries the digest of the `Dockerfile`'s builder image (#83).

### Added

- **`make check-devcontainer-base`**, part of `make lint`, fails when the
  devcontainer is not built from the `Dockerfile`'s builder image, digest
  included.
