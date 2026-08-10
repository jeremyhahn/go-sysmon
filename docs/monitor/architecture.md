# Monitor architecture

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

## Changing the interval

`SetInterval(d)` pushes the new duration onto a channel of capacity 1. If a
change is already pending, it gets replaced, so the last value wins rather than
the loop working through a backlog of interval changes someone made by dragging
through a dropdown. Safe to call from any goroutine.

Clients may pick 250ms, 500ms, 1s, 5s, 10s, 15s, 30s or 60s via POST
/api/interval.

## Thread safety

No mutexes:

- `running` — `atomic.Bool`, flipped with `CompareAndSwap`
- `subscribers` — `sync.Map`, which is a good fit for a read-heavy set
- `nextID` — `atomic.Uint64`
- `latest` — `atomic.Pointer[types.Snapshot]`
- `intervalCh` — buffered channel with replace-on-full

The lifecycle around start and stop *is* guarded, because "is it running" and
"cancel the context" have to move together — an early version raced there and
could call a nil cancel func.
