# Collector — implementation contract

> **Status: NORMATIVE** · Scope: `pkg/collector` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

What is read from where is in [architecture.md](architecture.md),
[sensors.md](sensors.md), [disk-and-network.md](disk-and-network.md) and
[virtualization.md](virtualization.md). This page is the contract.

## Exported surface

| Symbol | Contract |
|---|---|
| `Collector` | `Collect() error` — implemented by each sub-collector (`HostCollector`, `CPUCollector`, `NPUCollector`, `MemoryCollector`, `DiskCollector`, `NetworkCollector`, `ProcessCollector`, `LoadCollector`, `SensorCollector`, `VirtCollector`, `RuntimeCollector`); `GPUCollector.Collect(ctx)` takes a context |
| `NewSystemCollector(logger *slog.Logger) *SystemCollector` | Builds every sub-collector. A nil logger means `slog.Default()`. Tiering is on |
| `(*SystemCollector).Snapshot(ctx) (types.Snapshot, error)` | Runs the collectors due this tick and assembles one `types.Snapshot` |
| `(*SystemCollector).SetTiering(bool)` | Off runs every collector on every call (CLI `--refresh`, one-shot) |
| `(*SystemCollector).WaitForRuntimeDiskUsage` | Lets a caller wait for the runtime data-root measurement |

Each sub-collector has a `New<Name>Collector(logger)` constructor. The monitor
reaches the collector only through its `Snapshotter` interface
([monitor/engineering.md](../monitor/engineering.md)).

## Invariants

- **Tiers.** `host`, `cpu`, `memory`, `network`, `load` and virtualization
  detection run every tick. `gpu`, `process`, `sensor`, `virt` and the runtime
  data-root measurement run every 5th tick. `disk`, `npu` and the runtime
  inventory (`runtime`) run every 30th. The first snapshot runs everything.
  Skipped sub-collectors keep serving their last result, held in atomic
  pointers, so the snapshot is complete but not always new.
- **Failure isolation.** A sub-collector error is logged as `collector warning`
  with the collector's name and the snapshot is still assembled from what
  succeeded.
- **Cancellation.** A cancelled context returns the partial snapshot and a
  `types.CollectorError{Collector: "system"}`.
- **Read-only.** Nothing is installed, written, bound or loaded on the host.
  A reading that is unavailable (a DIMM sensor driver not bound, SMART
  refused) is reported as unavailable, not as zero.
- **No runtime socket** except `RuntimeCollector`/`images`, the one opt-in path
  to the container runtime API.
- **SMART** is read through ATA/NVMe passthrough, never `HDIO_DRIVE_CMD`.
- Rates are derived from counters between two readings; the first reading has
  none.

## Error semantics

Errors are typed values from `pkg/types` (`CollectorError` and the per-source
errors); sub-collector errors are logged and not propagated beyond the snapshot
assembly, except the cancellation case above.

## Configuration

No file or flags. Behaviour is selected by build tags (for example `nvml`; see
[testing/build-shapes.md](../testing/build-shapes.md)) and by
the privileges of the process. Files such as `gpu_nonvml.go` and
`megaraid_unsupported.go` stand in when a facility is not compiled in.

## Test strategy

Unit tests run against fixtures under `pkg/collector/testdata` and in-package
test doubles, so they need no hardware, privilege or runtime socket. `make test-collector` runs the package in every
build shape; `make coverage-collector` enforces the 90% floor;
`make bench-collector` runs the benchmarks; `make integration-test-collector`
runs the integration suite in the devcontainer
([testing/integration.md](../testing/integration.md)).

## Observability

`slog` records from the logger passed to `NewSystemCollector`, notably the
`collector warning` event above.
