# CI failure artifacts

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the artifacts CI keeps on failure.

When a CI job fails, it uploads what it knows about the failure as an artifact,
on Gitea and on github.com alike (#225). A passing job uploads nothing to Gitea.
Every failure artifact is kept for 7 days on github.com and 6 on Gitea (see
below).

| Artifact | Job | Workflow | What is in it |
|---|---|---|---|
| `playwright-results` | Browser Tests | `ci.yml` | `cmd/sysmon/frontend/test-results/`: for each failed test, `test-failed-1.png` (the screenshot), `error-context.md` (the error and an accessibility snapshot of the page) and, from the retry, `trace.zip` |
| `unit-test-logs` | Unit Tests (race) | `ci.yml` | `unit-<shape>.log`, one per [build shape](build-shapes.md) that ran. `make test` stops at the first failing shape, so the last log is the one that failed |
| `fips-test-logs` | FIPS | `ci.yml` | `fips.log` and `fips-prometheus.log` |
| `integration-test-logs` | Integration Tests | `ci.yml` | `integration.log`, the containerised suite's output |
| `desktop-session-logs` | Cinnamon Applet (real session) | `desktop.yml` | `cinnamon.log`, `xorg.log`, `system-bus.log`, `udisksd.log`, and the runner's own output as `run.log` ([desktop testing](desktop.md#the-sessions-logs)) |

Two success-path artifacts, `coverage` and `binaries`, are uploaded on
github.com only.

## Where the files come from

The Go suites and the desktop session write their logs to `TEST_LOG_DIR`
(`test-logs/` by default, git-ignored) as well as to the terminal, so a local
`make test`, `make test-fips`, `make integration-test` or
`make test-extensions-desktop` leaves the same files the CI artifact would
hold. The Browser Tests artifact is Playwright's own output directory.
`playwright.config.ts` sets `screenshot: 'only-on-failure'` and
`trace: 'on-first-retry'`. Playwright writes `error-context.md` for every
failed test without being asked.

## Downloading from Gitea

In the UI, open the run (the repository's Actions tab, then the run). Its
artifacts are listed under the jobs, and each one downloads as a zip.

Through the API (`$GITEA_TOKEN` is any token that can read the repository).
A pull request's runs carry no branch name, so find them by the commit:

```sh
API=http://git.example.internal/api/v1/repos/jeremyhahn/go-sysmon
SHA=$(git rev-parse HEAD)   # the full SHA: the filter does not match a short one

# The CI run for that commit.
curl -s -H "Authorization: token $GITEA_TOKEN" \
  "$API/actions/runs?head_sha=$SHA&event=pull_request" |
  jq '.workflow_runs[] | select(.path | startswith("ci.yml")) | {id, status, conclusion}'

# Its artifacts.
curl -s -H "Authorization: token $GITEA_TOKEN" "$API/actions/runs/<run id>/artifacts" |
  jq '.artifacts[] | {id, name, size_in_bytes, expires_at}'

# One artifact, as a zip. The URL redirects to the file, so -L.
curl -sL -H "Authorization: token $GITEA_TOKEN" -o playwright-results.zip \
  "$API/actions/artifacts/<artifact id>/zip"
```

Open a trace with `npx playwright show-trace trace.zip` from
`cmd/sysmon/frontend`, or at trace.playwright.dev.

Gitea keeps an artifact one day less than `retention-days` asks for. Gitea
1.27 turns the requested expiry back into whole days by rounding down, and
the request is made a moment after the job computes it. So 7 becomes 6, and
1 expires at once. That is why no upload here asks for fewer than 7 days.

## Downloading from github.com

The run's summary page lists the artifacts. From the command line:
`gh run download <run id> -n playwright-results`.

## One action on both planes

Every upload uses the same `actions/upload-artifact` release, pinned like every
other action and kept at its latest major by `make check-latest`
([toolchain](toolchain.md)). That release speaks the v4 artifact protocol,
which is also the only one Gitea's API lists and serves. Artifacts from
`upload-artifact@v3` reach Gitea's UI but never its API, and github.com has
refused v3 outright since January 2025. So v3 cannot be the answer for either
plane.

The v4 toolkit refuses any server that is not github.com, with
`GHESNotSupportedError`. Gitea's runner (`gitea/runner` v3.1.0 and later)
removes that refusal from the action's bundle when it downloads the action. It
also points `ACTIONS_RESULTS_URL` at its own cache server, which forwards the
artifact service to Gitea. The workflows therefore need no Gitea-specific step.
They do depend on the runner host being healthy:

| Upload step fails with | Cause on the runner host |
|---|---|
| `Failed to CreateArtifact: Unable to make request: ECONNREFUSED` | The runner's cache server (port 34567 on the host) is not reachable from job containers |
| `GHESNotSupportedError` | The runner restored the original bundle after an upload failed. A `skip` marker in the action's `.toolkit-patch` directory stops the runner patching it again, and the marker has to be removed once the cause is fixed |

Both are runner infrastructure problems, not problems with this repository.
Falling back would not help. Gitea's API serves only v4 artifacts, which
rules out v3. A fork of the action without the refusal would still send its
upload to the unreachable `ACTIONS_RESULTS_URL`. The fix is a healthy runner.
