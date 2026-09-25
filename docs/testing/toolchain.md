# Staying on the latest toolchain

Owner rule, 2026-09-22: go-sysmon uses the latest stable release of everything
it pins. That covers Go, the dev tools, Node.js, the Debian and Ubuntu base
images, the workflows' actions, and the GitHub-hosted runner labels. Two checks enforce it (#183), and a daily
workflow acts on the second.

| Check | Network | Where it runs | Fails when |
|---|---|---|---|
| `make check-pins` | offline | `make lint`, so every PR | a version reference disagrees with its pin |
| `make check-latest` | online | by hand, and the daily workflow | a pin is not the latest stable release |

## One source per version

| Pin | Source | Read by | Repeated in (checked by check-pins) |
|---|---|---|---|
| Go | `go` directive in `go.mod` | setup-go `go-version-file` | `golang:` image tags |
| Dev tools | `*_VERSION` in the `Makefile` (module in `*_MOD`, listed in `PINNED_TOOLS`) | `make tools` and every lint and scan target | none |
| Node.js | `.node-version` (`NODE_VERSION_FILE`) | setup-node `node-version-file` | `node:` image tags |
| Debian | `DEBIAN_RELEASE` in the `Makefile` | none | `debian:` tags, and the base variant of `golang:` and `node:` tags |
| Ubuntu | `UBUNTU_RELEASE` in the `Makefile` | none | `ubuntu:` tags |
| Hosted runners | `HOSTED_RUNNER_RELEASE` in the `Makefile` | none | every `ubuntu-NN.NN[-arm]` label in a workflow |
| Actions | each `uses:` ref | the runner | none: every ref is checked on its own |

Every pin names one exact release (`1.27.1`, `24.21.0`, `trixie`, `26.04`),
never a floating one. Node.js is pinned to a patch release, like Go, so the
image and CI build with the same Node.js.

## check-pins

`cmd/checkpins` walks the repository through an `os.Root`, skipping `.git`,
`node_modules`, `vendor` and `testdata`. It checks:

- Every `FROM` of an official `golang`, `node`, `debian` or `ubuntu` image in a
  Dockerfile. A build argument in the tag resolves from an `ARG` default in the
  same file.
- The same images in compose files, and in workflow job containers, service
  containers and `docker://` steps.
- Every `actions/setup-go` and `actions/setup-node` step. Each must read the
  source file (`go.mod`, `.node-version`) or name the exact release.
  `${{ env.NAME }}` resolves from the step, job or workflow `env`.
- Every versioned GitHub-hosted runner label (`ubuntu-26.04`,
  `ubuntu-26.04-arm`) anywhere in a workflow's values: `runs-on`, a matrix
  entry, or a quoted label inside a `runs-on` expression. `ubuntu-latest` is
  not one. On the Gitea plane it is a self-hosted runner label, which is
  company infrastructure.

A floating tag fails, and so do these:

- a tag with no base variant (`golang:1.27.1`), whose Debian release floats
- a setup step with no version input
- an expression only a runner can evaluate (`${{ matrix.go }}`)

To bump a pin, change its source and run `make check-pins`. It names every file
still on the old release.

## check-latest

`cmd/checklatest` looks up the latest stable release of every pin:

| Pin | Latest from | Passes when |
|---|---|---|
| Go | `https://go.dev/VERSION?m=text` | the directive is that release |
| Tool | `go list -m -json <module>@latest`, through `GOPROXY` like `go install`; skips retracted versions | the pin is that version |
| Node.js | `https://nodejs.org/dist/index.json`, the newest release on an LTS line | `.node-version` is that release |
| Debian | `deb.debian.org/debian/dists/stable/Release` | `DEBIAN_RELEASE` is stable's codename |
| Ubuntu | Launchpad's series list, the newest released `YY.04` of an even year | `UBUNTU_RELEASE` is that release |
| Hosted runner | the `actions/runner-images` README table, the newest GA `ubuntu-NN.04[-arm]` label of the same architecture | the label is that one |
| Action | `git ls-remote --tags https://github.com/<owner>/<repo>`, the highest stable `vX.Y.Z` tag | see below |

**Node.js follows LTS, not Current.** Current (26 while 24 is LTS) changes
incompatibly from one major to the next within months, and Node.js does not
recommend it for production. The newest LTS line is always the Active one.
Older LTS lines are in Maintenance, so their releases are never the newest LTS
release.

**Hosted runners follow GitHub's GA images, not Ubuntu's releases.** An image
badged beta or preview in the runner-images table is not generally available
and never counts as the latest; pinning one is `prerelease`. A label the table
does not list gets a typed error and no verdict: GitHub offers no such runner,
and a job asking for one waits forever. check-latest judges the pinned release
on both architectures, and every label a workflow names.

**Actions keep their pin's form.** This repository pins major tags (`@v7`), as
the company workflow templates do. The one exception is the template-owned
`opencode-review.yml`, which pins a commit SHA with a version comment.

- A major tag is current while the latest release is on its line: `v7` passes
  against `v7.0.1` and fails against `v8.0.0`.
- A minor tag is compared the same way at minor precision.
- An exact tag is compared exactly.
- A SHA pin is the newest release tag that points at that commit (annotated
  tags resolve through their peeled `^{}` line), compared exactly.
- A branch, or a commit no release points at, gets no verdict.

`git ls-remote` needs no API token and has no rate limit.

| Verdict | Meaning | Passes |
|---|---|---|
| `current` | the pin is the latest stable release | yes |
| `behind` | a newer stable release exists | no |
| `ahead` | the pin is newer than the latest reported, so it was retracted or never released | no |
| `prerelease` | the pin is an rc, a beta, a pseudo-version, Debian testing, or a preview runner image | no |

The command exits 0 when everything is current, 1 when something lags and 2
when an item could not be checked. Through `make`, either failure is a failed
target, and the message says which. An item whose latest release is unknown
gets no verdict and is never reported as current. That covers being offline, a
source that is down, and an answer that is not a version.

Comparison rules, all unit-tested in `internal/pins`:

- Go versions compare with the standard library's `go/version`. `1.27.1` and
  `go1.27.1` are the same release, a vendor suffix (`go1.27.1-bigcorp`) is
  ignored, and a bare language version (`1.27`) is behind any `1.27.x`.
- Module, Node.js and action versions compare as semantic versions
  (`golang.org/x/mod/semver`). `v1.7.12` is newer than `v1.7.9`, and build
  metadata does not count in the order.
- Ubuntu releases compare as fixed-width `YY.MM`. They are not semantic
  versions, because semver rejects the leading zero in `04`.
- Debian codenames do not order, so a pin that is not stable is placed by its
  own `Release` file. A lower major version is behind, and no version at all
  (testing) is prerelease.
- A source that reports an rc, or a bare language version, as the latest Go is
  refused as an invalid answer. It is not trusted.

### Mirrors and air-gapped runs

Each source is fetched from a URL a flag names. Every flag defaults to the
public source in the table above, so `make check-latest` passes none:

| Flag | Default | Accepts |
|---|---|---|
| `-go-releases-url` | `https://go.dev/VERSION?m=text` | `http`, `https` |
| `-node-index-url` | `https://nodejs.org/dist/index.json` | `http`, `https` |
| `-debian-dists-url` | `https://deb.debian.org/debian/dists` | `http`, `https` |
| `-ubuntu-series-url` | `https://api.launchpad.net/devel/ubuntu/series` | `http`, `https` |
| `-runner-images-url` | the `actions/runner-images` README on `raw.githubusercontent.com` | `http`, `https` |
| `-actions-base-url` | `https://github.com` | `http`, `https`, `file` |

A mirror serves the same documents at the paths the defaults use: the Debian
URL is the `dists` directory, under which `<suite>/Release` is read, and the
actions URL is where `<owner>/<repo>` is listed with `git ls-remote`. A `file`
URL there names a directory of repository mirrors, which is how an air-gapped
host checks its actions. Tool versions come from `go list -m`, so they follow
`GOPROXY` rather than a flag.

A URL that is not absolute, has no host, or uses a scheme the source cannot be
fetched over is a usage error (exit 2) before anything is fetched.

The integration suite (`test/integration/tools`) runs the command this way,
against in-process stand-ins for every source, a module proxy and a Gitea API.

check-latest is not in `make lint` or `ci.yml`. An upstream release is not a
defect in whatever PR happens to be open, and it must not fail one.

## The daily workflow

`.github/workflows/check-latest.yml` runs `make check-latest` at 05:17 UTC
with `-file-issue`. When something lags, it files one issue as the project
agent, using the `AGENT_TOKEN` secret, as `automerge.yml` does (approved 2026-09-24):

- The issue is labelled `task:ready`. The label is read back after filing, and
  applied again if Gitea dropped it.
- It files nothing while an earlier issue is open. It finds that issue by a
  marker comment in the body, so editing the title does not produce a
  duplicate.
- The run fails only when an item could not be checked. A lag goes into the
  issue and the run stays green.

It also runs on `workflow_dispatch`, with a `dry_run` input, and on pull
requests that change the check itself. Pull-request runs are always dry runs.
A dry run still looks for an open issue, then prints the issue it would file
and files nothing. To see one locally:

```bash
GITEA_TOKEN=... make check-latest CHECK_LATEST_FLAGS="-file-issue -dry-run \
  -gitea-api http://git.example.internal/api/v1 -repo jeremyhahn/go-sysmon"
```

The job does not run on the github.com mirror, because that mirror has neither
the Gitea API nor the agent's token.
