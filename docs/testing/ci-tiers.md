# CI tiers

> **Status: REFERENCE** · Last verified: 2026-11-23
> As-built description of when each `ci.yml` job runs (#267, handbook
> `ci-standard.md` §8).

`ci.yml` runs in two tiers. The PR tier is what every pull request waits on.
The full tier adds the slow and the network-bound suites, and runs where a red
result is worth the wait: on `main`, on demand, and on a pull request that asks
for it.

| Tier | Runs on | Jobs (check names) |
|---|---|---|
| PR | every `pull_request` event (`opened`, `synchronize`, `reopened`), and every full-tier run | Format, Vet & Lint · Unit Tests (race) · Build Binaries · Bundle Currency |
| Full | a push to `main`; `workflow_dispatch`; a `pull_request` whose PR carries the `ci:full` label | PR tier, plus Security Scan · FIPS · Integration Tests · Browser Tests · Desktop Extension Tests · Container Image |

**CI Passed** `needs` every job in both tiers and is the only required check. A
PR-tier job must succeed. A full-tier job must succeed too, except that it may
be skipped in a run that did not ask for the full tier. A full-tier job skipped
in a full-tier run fails the gate, as does any failed, cancelled or unreadable
result.

The unit suite runs once, under `-race` (`make test`), in the PR tier.

The latest-version check is its own workflow, `check-latest.yml`, run daily on
a schedule, with `workflow_dispatch`, and on a PR that changes the check
([toolchain](toolchain.md)).

## Running the full tier on a pull request

Put the `ci:full` label on the PR when it is opened; every later push then runs
the full tier while the label stays on. Adding the label to an open PR starts no
run by itself (the next push picks it up): `ci.yml` does not listen for
`labeled` or `unlabeled`, which would start a run per label change. To run the
full tier on a PR that is already open, dispatch `ci.yml` with
`workflow_dispatch` on its branch.

The workflow reads the label from the PR's label list
(`github.event.pull_request.labels.*.name`), never from `github.event.label`,
which only a `labeled` event carries.

## Scheduled runs

`check-latest.yml` is the only workflow with a `schedule:` trigger: it runs
daily at 05:17 UTC. `desktop.yml`'s real Cinnamon session and a nightly
full-tier run of `ci.yml` are started with `workflow_dispatch`.
