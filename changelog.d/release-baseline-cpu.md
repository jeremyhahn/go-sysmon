### Fixed

- **The `linux-amd64` release binaries start on every x86-64 CPU again.**
  v0.3.4-alpha and v0.3.5-alpha needed a CPU with AVX2 (x86-64-v3). On an
  older one, such as an Intel Atom, glibc refused them with `CPU ISA level is
  lower than required`. They were built on GitHub's `ubuntu-26.04` runner,
  whose C toolchain targets x86-64-v3. `release.yml` now builds the server and
  desktop binaries, `amd64` and `arm64`, inside the `Dockerfile`'s builder
  image. Nothing changes on a newer CPU: Go builds for `GOAMD64=v1` as before,
  and its runtime still uses newer instructions where it detects them. The
  container image was not affected.

### Added

- **`make check-cpu-baseline`** fails when a binary's ELF notes demand a CPU
  above its architecture's baseline. `release.yml` runs it on the binaries it
  has built, before they are uploaded. To check any other binary, run `make
  check-cpu-baseline BINARIES=bin/sysmon-server`.
- **`make check-release-builder`**, part of `make lint`, fails when
  `release.yml` would build the release binaries in any image but the
  `Dockerfile`'s builder image, digest included.
