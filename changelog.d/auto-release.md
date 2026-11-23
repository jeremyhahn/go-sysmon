### Changed

- **A green merge to `main` that bumps `VERSION` publishes the release.**
  `auto-release.yml` runs when CI passes on `main`. If `v$VERSION` is not a
  tag yet, it checks the changelog and the version order, tags the commit and
  calls `release.yml`, which publishes the binaries, the GitHub release and the
  `ghcr.io` image. Merges that leave `VERSION` alone publish nothing.
- **Before 1.0, every release is the Latest release and `:latest`.** An
  `-alpha` used to be a GitHub pre-release that never moved the image's
  `:latest`, so both stayed on 0.1.2. From 1.0 on only a plain `vX.Y.Z` does.
  The install examples now follow the latest release instead of naming one.
- **Dependabot keeps Go modules, frontend packages, GitHub Actions and base
  images current**, with one grouped PR per ecosystem a day.
