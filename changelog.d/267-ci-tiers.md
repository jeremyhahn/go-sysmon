### Changed

- **CI runs in two tiers.** Every pull request runs the PR tier: Format, Vet &
  Lint, Unit Tests (race), Build Binaries and Bundle Currency. The full tier
  adds Security Scan, FIPS, Integration Tests, Browser Tests, Desktop Extension
  Tests and Container Image, and runs on a push to `main`, on
  `workflow_dispatch`, and on a pull request that carries the `ci:full` label
  when it is opened. Adding the label later starts no run; dispatch the
  workflow on the PR's branch instead. CI Passed accepts a full-tier job as
  skipped only in a run that did not ask for the full tier (#267).
- **No scheduled workflows.** The desktop session tests and the toolchain
  currency check lost their `schedule:` triggers; their nightly runs are
  dispatched with `workflow_dispatch` (#267).
