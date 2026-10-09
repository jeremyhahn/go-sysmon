# Monitor architecture

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the monitor package.

`pkg/monitor` sits between the collector and everything that wants data — the
web server, the GUI, the tray, the CLI's refresh mode. It polls on an interval
and hands each snapshot to every subscriber.

The point of the layer is that collection happens once regardless of how many
consumers there are. Five browser tabs on the dashboard produce one poll, not
five.

```
+-------------------+         +-----------+         +------------------+
|  SystemCollector  |         |  Monitor  |         |   Subscribers    |
|  (Snapshotter)    +-------->+  (poll)   +-------->+   Event stream   |
|                   |         |           |         |  GUI binding     |
+-------------------+         +-----+-----+         |  System tray     |
                                    |               +------------------+
                                    v
                              atomic.Pointer
                             (latest snapshot)
```

## The Snapshotter interface

The monitor takes anything that can produce a snapshot:

```go
type Snapshotter interface {
    Snapshot() (*types.Snapshot, error)
}
```

`SystemCollector` takes a `context.Context`, so a thin `snapshotterAdapter`
bridges the two. Keeping the interface this narrow is what makes the monitor
testable without touching hardware.

## The poll loop

A `time.Ticker` in a goroutine. On each tick it calls `Snapshot()`, stores the
result in an `atomic.Pointer`, and broadcasts. It also watches `intervalCh` and
resets the ticker immediately when a new interval arrives, so changing the
dropdown in the UI takes effect on the next frame rather than after the old
interval expires.

## Subscribers

`Subscribe()` hands back:

- `ID`, a monotonically increasing `uint64`
- `Ch`, a buffered channel of capacity 4
- `Done`, closed when the subscriber goes away or the monitor stops

Broadcast sends are non-blocking. If a subscriber's buffer is full, the oldest
snapshot is dropped to make room for the newest.

That is deliberate. A monitor is only interesting in the present tense: a slow
consumer that falls behind wants the current reading, not a queue of stale
ones. And more importantly, one wedged event stream must never stall
the loop for everyone else.

### Ordering

A publication is stored before it is broadcast, and publications are
serialised: each one finishes its broadcast before the next is stored. Two
things follow, and consumers rely on both.

- A subscriber that has been sent a snapshot can read it back through
  `Snapshot()`. Test helpers that wait for a publication subscribe first, then
  check `Snapshot()`, then wait on the channel.
- A subscription taken while a publication is being broadcast may be sent that
  publication, even though `Snapshot()` already answered with it. It is never
  sent an older one. So a consumer that reads `Snapshot()` and then subscribes
  can receive the same reading once more, and must not count it as a new
  publication (#214).

## Changing the interval

`SetInterval(d)` pushes the new duration onto a channel of capacity 1. If a
change is already pending, it gets replaced, so the last value wins rather than
the loop working through a backlog of interval changes someone made by dragging
through a dropdown. Safe to call from any goroutine.

Clients may pick 250ms, 500ms, 1s, 5s, 10s, 15s, 30s or 60s via POST
/api/interval.

## Thread safety

- `subscribers` — `sync.Map`, which is a good fit for a read-heavy set
- `nextID`, `generation`, `intervalNs` — atomics
- `latest` — `atomic.Pointer` to the published reading and the baseline of the
  one before it, swapped as a pair so a reader never mixes two ticks
- `intervalCh` — buffered channel with replace-on-full
- `mu` — guards the start/stop lifecycle, because "is it running" and "cancel
  the context" have to move together; an early version raced there and could
  call a nil cancel func
- `pubMu` — serialises publishing: a new collection, and a re-decoration of the
  published one by `SetDecorator` or `Restamp`. Collection runs outside it, so
  a hung collector never blocks a rename

Reading the published snapshot takes no lock.
