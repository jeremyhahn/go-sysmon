### Fixed

- **The README's binary install works on arm64.** It downloaded the
  `linux-amd64` build on every host, although each release also ships
  `linux-arm64` binaries. It now picks the build from `uname -m`, checks it
  against its `.sha256` file before installing, and says that the container
  image covers both architectures.
