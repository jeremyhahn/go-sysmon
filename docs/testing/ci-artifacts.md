# CI failure artifacts

> **Status: REFERENCE** · Last verified: 2026-11-23
> As-built description of the artifacts CI keeps on failure.

When a CI job fails, it uploads what it knows about the failure as an artifact
(#225). Every artifact is kept for 7 days.

| Artifact | Job | Workflow | What is in it |
|---|---|---|---|
| `playwright-results` | Browser Tests | `ci.yml` | `cmd/sysmon/frontend/test-results/`: for each failed test, `test-failed-1.png` (the screenshot), `error-context.md` (the error and an accessibility snapshot of the page) and, from the retry, `trace.zip` |
| `unit-test-logs` | Unit Tests (race) | `ci.yml` | `unit-<shape>.log`, one per [build shape](build-shapes.md) that ran. `make test` stops at the first failing shape, so the last log is the one that failed |
| `fips-test-logs` | FIPS | `ci.yml` | `fips.log` and `fips-prometheus.log` |
| `integration-test-logs` | Integration Tests | `ci.yml` | `integration.log`, the containerised suite's output |
| `desktop-session-logs` | Cinnamon Applet (real session) | `desktop.yml` | `cinnamon.log`, `xorg.log`, `system-bus.log`, `udisksd.log`, and the runner's own output as `run.log` ([desktop testing](desktop.md#the-sessions-logs)) |

Two success-path artifacts, `coverage` and `binaries`, are uploaded by every
run of their jobs.

## Where the files come from

The Go suites and the desktop session write their logs to `TEST_LOG_DIR`
(`test-logs/` by default, git-ignored) as well as to the terminal, so a local
`make test`, `make test-fips`, `make integration-test` or
`make test-extensions-desktop` leaves the same files the CI artifact would
hold. The Browser Tests artifact is Playwright's own output directory.
`playwright.config.ts` sets `screenshot: 'only-on-failure'` and
`trace: 'on-first-retry'`. Playwright writes `error-context.md` for every
failed test without being asked.

## Downloading

The run's summary page lists the artifacts, and each one downloads as a zip.
From the command line:

```sh
gh run list --workflow ci.yml --commit "$(git rev-parse HEAD)"   # find the run
gh run download <run id> -n playwright-results
```

Open a trace with `npx playwright show-trace trace.zip` from
`cmd/sysmon/frontend`, or at trace.playwright.dev.

## The upload action

Every upload uses the same `actions/upload-artifact` release, pinned like every
other action and kept at its latest major by `make check-latest`
([toolchain](toolchain.md)).
