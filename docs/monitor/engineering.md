# Monitor — implementation contract

> **Status: NORMATIVE** · Scope: `pkg/monitor` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

Design, subscriber semantics and the thread-safety inventory are in
[architecture.md](architecture.md). This page lists the surface and the errors.

## Exported surface

| Symbol | Contract |
|---|---|
| `DefaultInterval` | `time.Second` |
| `Snapshotter` | `Snapshot() (*types.Snapshot, error)` — the only thing the monitor needs from a collector |
| `Decorator` | `Identify(*types.Snapshot)` sets node and build fields and must depend only on current configuration; `Evaluate(*types.Snapshot)` sets alerts, is called exactly once per snapshot in collection order, and may keep state |
| `New(s Snapshotter, interval time.Duration) (*Monitor, error)` | `InvalidIntervalError` if `interval <= 0` |
| `(*Monitor).Start() error` | Begins polling; `MonitorAlreadyRunningError` if running. Waits out a stop in progress. Clears the previous run's latest snapshot and advances the generation |
| `(*Monitor).Stop() error` | Cancels the loop, waits for it to exit, then closes the `Done` of that run's subscribers; `MonitorNotRunningError` if not running. The last reading stays readable |
| `(*Monitor).Running() bool`, `Interval() time.Duration`, `Generation() uint64` | `Interval` and `Generation` are atomic reads; `Running` takes the lifecycle mutex |
| `(*Monitor).Snapshot() (*types.Snapshot, error)` | The latest published reading |
| `(*Monitor).Consecutive() (prev, cur *types.Snapshot, err error)` | The latest reading and the one before it, as a pair; `NoSnapshotYetError` while running with nothing collected, `MonitorNotRunningError` when stopped with nothing |
| `(*Monitor).Subscribe() *Subscriber` / `Unsubscribe(id uint64)` | `Subscriber{ID uint64, Ch chan *types.Snapshot (cap 4), Done chan struct{}}`; `Chan()` accessor |
| `(*Monitor).SetInterval(d time.Duration) error` | `InvalidIntervalError` if `d <= 0`; takes effect on the loop's next iteration, last write wins |
| `(*Monitor).SetDecorator(d Decorator) error` / `Decorator()` | One decorator per monitor. `nil` clears. The same value again is a no-op; a different one is `DecoratorInstalledError`. Installing re-decorates the published snapshot |
| `(*Monitor).Restamp()` | No return value. Re-applies the decorator's `Identify` to the published snapshot and broadcasts it before returning; a no-op when nothing changed |

Errors are typed values from `pkg/types`; none is built with `fmt.Errorf`.

## Invariants

- Collection happens once per tick regardless of subscriber count.
- A publication is stored, then broadcast, and publications are serialised
  under `pubMu`. Collection runs outside that lock.
- A subscriber sent a snapshot can read it back through `Snapshot()`; a
  subscription taken mid-broadcast may be sent the current reading again, never
  an older one (#214).
- Broadcast never blocks: a full subscriber buffer drops its oldest snapshot.
- A restamp is the same reading (same `timestamp`, counters and alerts), not a
  new one.
- A monitor that reaches `Stop` closes `Done` only for subscribers of the run
  that ended (generation match).
- A snapshot whose Snapshotter left `timestamp` unset is dated by the monitor.

## Error semantics

Every error in the table is returned to the caller. A failing
`Snapshotter.Snapshot` is non-fatal: that tick publishes nothing, the loop
continues, and the previous reading stays current. The monitor does not log it;
the collector does.

## Configuration

None of its own. The caller passes the interval; `sysmon serve` takes it from
`server.interval_ms` and retunes it on reload.

## Test strategy

`pkg/monitor` is unit-tested with a fake `Snapshotter` and an injectable ticker
(`newTicker`), so no test sleeps on wall-clock time or touches hardware. Run it
with `make test-monitor`; its integration suite is
`make integration-test-monitor` (see [testing/integration.md](../testing/integration.md)).

## Observability

No metrics or log events of its own; consumers observe it through the
snapshot's `timestamp` and the server's stream.
