# Coverage

> **Status: NORMATIVE** · Scope: coverage floors and measurement · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

Every package is held to a 90% statement-coverage floor, for its unit tests
and for the integration suite (CLAUDE.md, #167). This page says how coverage
is measured, which make targets measure it, and where each package stands.

## How it is measured

`scripts/pkgtest.sh` does the measuring for every target below.

- **Unit coverage** runs the package's own tests once per distinct build
  among the test shapes ([build shapes](build-shapes.md)). A file behind a
  build tag is compiled, and so measured, only in the shapes that set the tag.
  Each run writes Go's binary coverage data (`-test.gocoverdir`), and the runs
  are merged. A block covered in any shape counts as covered. Shapes that
  compile the package from the same files are one build, and run once.
- **Integration coverage** runs the integration suite in the devcontainer
  ([integration tests](integration.md)).
  - The test binary is built with `-cover -coverpkg=<module>/...`.
  - When `SYSMON_COVERDIR` is set, `TestMain` builds every `sysmon` binary
    the suite starts with the same flags.
  - Every process writes into one coverage directory, and the report merges
    them. A server under test is stopped with SIGTERM, not killed, so it
    exits normally and writes its data.
- **What counts:** the package's own statements. Test files are never
  instrumented. Mock files are excluded: a file whose name contains `mock`, or
  any file under a `mock` or `mocks` directory. A package compiled into no
  measured binary reports 0%.

## Make targets

The targets are generated from `go list`, one set per package. A new package
gets its targets with no Makefile edit, and `make check-package-targets`
(part of `make lint`) fails if one is ever missing. The same check fails if two
package directories map to one target name.

| Target | What it does |
|---|---|
| `make test-<pkg>` | Runs the package's unit tests under `-race`, once per distinct build |
| `make coverage-<pkg>` | Measures the package's unit coverage and fails below 90% |
| `make integration-test-<pkg>` | Runs the integration suite in the devcontainer and fails if the package's integration coverage is below 90% |
| `make bench-<pkg>` | Runs the package's benchmarks; exists for packages that have benchmarks |
| `make coverage` | `coverage-<pkg>` for every package |

`<pkg>` is the package directory with a leading `pkg/` or `internal/` dropped
and each remaining slash replaced by a dash. For example, `pkg/collector` is
`collector`, `internal/tsgen` is `tsgen`, `pkg/types/typestest` is
`types-typestest`, and `cmd/sysmon` is `cmd-sysmon`.

## Where each package stands

"Before" was measured on `release-candidate` at the start of #167
(2026-09-24), and "after" once #167's tests landed. Unit
coverage merges every test shape. Integration coverage comes from the suite in
the devcontainer on gpu-host.

- **No tests:** the package has no test files, so nothing measures it.
- **Not reached:** no binary the integration suite runs contains the package.

| Package | Unit (before) | Unit (after) | Integration (before) | Integration (after) |
|---|---|---|---|---|
| `cmd/buildtags` | no tests | 96.2% | not reached | 100.0% |
| `cmd/checklatest` | 41.2% | 90.3% | not reached | 97.4% |
| `cmd/checkpins` | 58.8% | 95.0% | not reached | 100.0% |
| `cmd/sysmon` | 74.5% | 90.2% | 45.7% | 89.1% (below the floor, owner-approved) |
| `cmd/tsgen` | no tests | 90.9% | not reached | 100.0% |
| `internal/await` | 100.0% | 100.0% | 33.3% | 33.3% (test helper, exempt) |
| `internal/buildtags` | 99.1% | 99.1% | not reached | 98.2% |
| `internal/deadpeer` | 90.0% | 90.0% | not reached | 38.3% (test helper, exempt) |
| `internal/gitea` | 92.3% | 92.3% | not reached | 94.5% |
| `internal/pins` | 94.7% | 94.7% | not reached | 95.1% |
| `internal/tsgen` | 91.6% | 91.6% | not reached | 92.1% |
| `pkg/alert` | 92.1% | 92.1% | 56.3% | 65.0% (below the floor, owner-approved) |
| `pkg/cli` | 95.9% | 95.9% | 65.9% | 90.2% (host-hardware, owner-approved) |
| `pkg/collector` | 87.3% | 91.8% | 45.3% | 85.0% (below the floor, owner-approved) |
| `pkg/config` | 91.5% | 91.5% | 20.8% | 91.1% |
| `pkg/delta` | 88.2% | 98.8% | 0.0% | 76.0% (below the floor, owner-approved) |
| `pkg/fleet` | 96.8% | 96.8% | 58.8% | 93.2% |
| `pkg/memlimit` | 94.7% | 94.7% | 49.1% | 80.8% (below the floor, owner-approved) |
| `pkg/metrics` | 100.0% | 100.0% | 0.0% | 100.0% |
| `pkg/monitor` | 98.8% | 98.8% | 86.7% | 91.3% |
| `pkg/nodename` | 100.0% | 100.0% | 20.0% | 90.0% |
| `pkg/outbound` | 95.8% | 95.8% | 8.3% | 95.8% |
| `pkg/prom` | 93.1% | 93.1% | not reached | 90.5% |
| `pkg/sdnotify` | 93.9% | 93.9% | 42.4% | 97.0% |
| `pkg/server` | 90.7% | 90.7% | 33.9% | 88.4% (below the floor, owner-approved) |
| `pkg/types` | 90.6% | 90.6% | 45.3% | 67.2% (below the floor, owner-approved) |
| `pkg/types/typestest` | 98.3% | 98.3% | not reached | 0.0% (test helper, exempt) |
| `pkg/watchdog` | 97.1% | 97.1% | 14.3% | 91.4% |

## Integration exceptions

`build/coverage-exceptions` is the one list of what the integration floor
leaves out, and `scripts/pkgtest.sh integration` reads it. Each entry gives a
path, a category, a reason and the owner's approval (2026-09-24).
`make check-coverage-exceptions`, part of `make lint`, rejects an entry that
is malformed, unapproved, of an unknown category, or names a path that no
longer exists.

- **Hardware:** the MegaRAID files and SATA SMART, which need devices the
  devcontainer does not have, and the SMART absence classifier, which needs
  each class of device it explains.
- **Host-hardware:** GPU, NPU and NVMe SMART. What these reach depends on the
  runner's hardware, so they are excepted to keep the gate's answer the same
  on every runner.
- **Host:** md software RAID. A test array would be a kernel-global device,
  which would modify the host.
- **GUI:** `cmd/sysmon/bindings.go`, reached only from a desktop session. The
  owner approved it explicitly.
- **Test helpers:** `internal/await`, `internal/deadpeer` and
  `pkg/types/typestest` ship in no binary and are exempt as packages.
- **Below the floor, by owner ruling:** `pkg/collector` and `pkg/alert`
  (hardware and host state a container host does not have), `pkg/cli` (it
  renders the host's own GPUs and NPUs, so what it reaches depends on the
  runner's hardware: 90.2% on gpu-host, about 77% without them), and `pkg/delta`,
  `pkg/server`, `pkg/memlimit`, `pkg/types` and `cmd/sysmon` (defensive
  branches: refusals of malformed input, write failures on dropped
  connections, kernel output that does not occur). The owner ruled that code is
  not reorganised to raise the number. Each is measured and reported on every
  run, with the figure it reached recorded in the file, and never fails the
  gate.

`make integration-test` fails when any other package is below 90%.

The exceptions apply to integration coverage only. Unit coverage has none:
every excepted file is still covered by its unit tests and fixtures, which
`make coverage` holds to 90%.
