### Fixed

- **The container image takes Debian's security fixes on every build.** The
  runtime stage runs `apt-get upgrade`, but CI and the release build restored
  that layer from the build cache, so a fix published after the layer was
  first built never reached the image and the image scan failed on it. Every
  build of the image now passes `no-cache-filters: runtime` (`--no-cache-filter
  runtime` in `make scan-image`), which rebuilds that one stage and keeps the
  cache for the rest.

### Added

- **`make check-runtime-uncached`**, part of `make lint`, fails when the
  `Dockerfile`'s final stage is not named `runtime` or a workflow builds the
  image without `no-cache-filters: runtime`.
