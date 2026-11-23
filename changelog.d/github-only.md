### Changed

- **GitHub is the only forge.** Every Gitea code path is gone: the Gitea
  release and image jobs in `release.yml`, the off-GitHub branches of the
  `ci.yml`, `release.yml` and `desktop.yml` conditionals, the
  `internal/gitea` package, and `.gitea/workflows` as a place `make
  check-pins` and `make check-latest` look for workflows. The jobs, runners
  and gates CI and releases run on GitHub are unchanged.
- **The daily check-latest run files its issue on GitHub.** The new
  `internal/github` package finds an open check-latest issue by the marker in
  its body and files one `task:ready` issue when none is open, with the
  workflow's own `GITHUB_TOKEN`. `check-latest.yml` runs on a daily schedule
  again. `cmd/checklatest` takes `-github-api` (default
  `https://api.github.com`) in place of `-gitea-api`, and reads its token from
  `GITHUB_TOKEN` by default.
- **Dependabot PRs merge themselves once CI is green.** The new
  `dependabot-automerge.yml` enables GitHub's native auto-merge on each
  Dependabot PR and replaces `automerge.yml`.

### Removed

- **The manual release targets.** `make release`, `make release-candidate`,
  `make release-alpha` and `scripts/release.sh` pushed a signed tag to the
  Gitea remote. A green merge that bumps `VERSION` releases instead
  (`auto-release.yml`).
