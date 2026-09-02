# Delta streaming

`GET /api/events?deltas=1` sends what changed instead of a whole snapshot.

A snapshot of a busy host is around 200–270 kB of JSON. About three quarters of
it is the process list, and most of the rest is hardware description that never
changes: CPU flags, DIMM part numbers, disk models, interface addresses. The
original stream encoded all of it separately for every connection on every tick.

## What it saves

Measured end to end against a real 24-core host with ~1000 processes, ten
seconds of stream per client:

| | uncompressed | gzipped |
|---|---|---|
| `GET /api/events` | 3.75 MB | 318 kB |
| `GET /api/events?deltas=1` | 551 kB | 71 kB |

On a synthetic snapshot where one process gets busy and the list rotates —
which is what a tick looks like, since the list is ordered by CPU usage:

| processes | full | delta |
|---|---|---|
| 150 | 40 kB | 2.0 kB (5.0%) |
| 1000 | 202 kB | 7.1 kB (3.5%) |

## What it costs

Per tick, on the same host (`go test ./pkg/server -bench BenchmarkFrame`):

| | |
|---|---|
| old protocol, per gzip client | 0.71 ms |
| old protocol, per plain client | 0.30 ms |
| new protocol, per gzip client | 0.006 ms |
| new protocol, once for everyone | 4.7 ms (1000 processes), 1.0 ms (150) |

The diff is computed once and the same bytes go to every client, so the server's
cost stops growing with the number of connections — but it is **not free below
that point**. The break-even is around six concurrent dashboards: with fewer,
the diff costs more CPU than the per-client encoding it replaces.

That is worth stating plainly rather than claiming a saving that is not there.
In context it is a small number either way: collecting a snapshot on the same
host costs over a full core-second per ten seconds, so 4.7 ms per tick is noise
beside it. The bytes are the real prize, and those are saved unconditionally —
which is what matters to a browser reaching the dashboard over a VPN.

The cost is also only paid while someone is watching. The hub subscribes to the
monitor on the first delta client and stops when the last one disconnects.

## The protocol

Two protocols share the endpoint. **Without the parameter nothing has changed**:
`event: snapshot`, payload the bare snapshot, exactly as documented before. Any
client written against that keeps working.

With `?deltas=1` the client declares it can apply patches:

```
event: full
data: {"seq": 41, "snapshot": { ... }}

event: delta
data: {"seq": 42, "patch": { ... }}

event: delta
data: {"seq": 43}
```

The event names differ from `snapshot` deliberately. A client that negotiated
one protocol and is reading the other fails at once rather than parsing an
envelope as a snapshot and rendering a page of undefined values.

A `delta` frame with no `patch` means nothing measurable moved. The sequence
still advances, and the client must accept it — treating it as a gap would
reconnect every time an idle machine stayed idle.

## When a whole snapshot is sent

A patch applies to exactly one frame: the one before it. The server sends a
`full` frame to any client that is not exactly one behind:

- on connection, before anything else;
- after a frame that client missed, which the server knows because it tracks
  what it wrote to each connection;
- every 60 frames regardless, so a copy that has drifted for a reason nobody
  predicted is corrected within a minute rather than staying wrong for as long
  as the tab is open.

The client checks too. A gap in the sequence, a patch that does not fit what it
holds, a patch arriving with no snapshot to apply it to, **or a frame that is
not a frame** all cause it to drop the stream and reopen — which begins with a
whole snapshot.

That last one is easy to leave out. `JSON.parse(data) as FullFrame` is a promise
to the compiler and not a check: it cannot fail at runtime, so `null`, a number,
`{}` or an envelope with no payload are all accepted, assigned to the rendered
state, and reported as connected and current. That is this protocol's own
failure arriving through the one door that was not looking — not a stale
reading, but no reading at all, presented as live truth. The client validates
the envelope strictly and checks that the payload is a measurement (it has a
timestamp and a host) before trusting any of it.

**The bar this meets: a missed frame never leaves stale values rendering as
current.** That is the failure class of a frozen peer reading — a number that
looks measured and live and is neither. Applying a patch across a gap is worse
than useless, because it produces a snapshot that never existed on the server
and looks entirely plausible. So it is refused, and refusing is loud: the
applier throws rather than merging what it can, and the transport resyncs.

## The patch format

A patch is a tree mirroring the snapshot's shape, holding only what changed.
Absence means unchanged. Exactly one field of a node says how to apply it:

| | |
|---|---|
| `{"r": value}` | replace this node outright |
| `{"o": {...}}` | an object: apply each named sub-patch, leave the rest |
| `{"i": {...}}` | an array of equal length: apply by position |
| `{"a": {...}}` | an array whose elements have an identity: apply by it |

**Exactly one** is a rule both receivers enforce, not a description of what the
sender happens to emit. A node carrying two operations is refused rather than
having one of them applied: a receiver that dispatched on the first field it
recognised would apply that one, discard the other, and report a tree the
sender never described as current — and which one won would depend on the order
of the branches, so the two implementations could disagree while both reported
success. Every pair is in the refusal fixture.

A field present and holding `null` is **not** an operation, and does not count
towards the one: `{"r": 1, "o": null}` says one thing to do and is applied. The
rule refuses two things to do, not two keys. `"r"` is the exception, because
`"r": null` is a replacement *with* null — the distinction the Go type carries a
value `json.RawMessage` for.

`"r"` present and holding `null` is a real change. It must be distinguished from
`"r"` being absent, which is why the Go type carries a value `json.RawMessage`
rather than a pointer — `encoding/json` decodes `null` into a pointer field by
setting it to nil, which is also how "no replacement" is spelled, and the two
would be indistinguishable after a round trip.

A keyed array patch is:

```json
{"a": {"k": "pid",
       "ord": ["3", "9", "1"],
       "p": {"3": {"o": {"cpu_percent": {"r": 30}}}},
       "n": {"9": {"pid": 9, "name": "new-process"}}}}
```

**An element must be the element the patch says it is**, on both paths. A
survivor's own key field has to still render to the identity it was addressed
under after its sub-patch is applied — a sub-patch can rewrite the key itself,
and the differ represents an identity change as a removal plus a new element,
never as a survivor changing its key.

An element in `n` must **be** the element it is filed under: its own key field
has to render to the identity it is placed at, and that identity must not
already be in the array. Its position is a claim, and nothing checked the claim
against the element — `{"ord":["2"],"n":{"2":{"pid":999}}}` placed an element
whose identity is 999 under the key 2 and reported the frame applied, leaving
the receiver holding an array the sender does not have.

`ord` is the authoritative membership when present: anything not named in it is
gone. It is sent only when the order changed — which, since adding or removing
an element changes it too, means it is present whenever membership did.

**`"ord": []` and no `ord` at all are different frames.** The first says the
array is now empty; the second says its membership did not change. They differ
by one pair of brackets and by the whole meaning of the frame, which is why
`KeyedPatch.Order` is a `*[]string` on the Go side rather than a slice with
`omitempty` — as a slice the two encoded identically, the last process exiting
was sent as "nothing changed about membership", and every receiver went on
rendering a process that had gone. Both fixtures cover it, and there is a
property test over three thousand random keyed arrays, membership included,
that applies each patch **through the wire** rather than in memory. The
original property test did encode; it never produced a keyed patch at all,
because its field names never sit at a path in `arrayKeys`.

### Why identity rather than position

The process list is ordered by CPU usage and reorders on almost every tick.
Matched by position, a rotation makes every surviving process look changed: on
the 1000-process fixture above that is the difference between a 7 kB delta and a
60 kB one. `arrayKeys` in `pkg/delta/delta.go` names the identity field for each
array — `pid`, `name`, `index`, `id` — and everything else falls back to
position, then to wholesale replacement. Those fallbacks are always correct; the
table only decides how small the result is.

`k` travels in the patch rather than being looked up on both sides. A client
with its own copy of the table would apply the wrong key the moment the two
drifted, and would do it silently.

## What the client checks before rendering

A frame that parsed is not a measurement. `transport.ts` validates the payload
of every `full` frame **and the result of every delta**, against the container
fields the `Snapshot` type declares anywhere in its tree — the timestamp, the
sections the panels read, and every list, including nested ones such as
`memory.dimms`, `processes.process_list`, `sensors.psi.*` and the
virtualization lists.

Both doors, because a patch whose root operation is a replacement produces a new
snapshot without a `full` frame ever arriving: `{"seq":2,"patch":{"r":null}}` is
contiguous and well formed, and a cast rather than a check would have rendered
the `null`.

`gpus` and `alerts` are deliberately **not** required, and that is measured
rather than assumed: a real snapshot from this build carries `"alerts": null` on
a healthy node, because a nil Go slice marshals to null. The store reads both
through `?? []`.

The shape is three dotted-path tables, **derived from the `Snapshot` type**
rather than from the fields anyone happened to think of. That distinction is
the lesson of this section: it was built by enumeration — the top level, then
`sensors`, then `memory.dimms` and the virtualization lists — and each round
was correct and short, because each listed what was in front of it. Reading the
type gives the whole set at once.

| Table | Rule |
|---|---|
| objects | must be an object; the Go types are structs, never pointers |
| required arrays | `cpus`, `disks`, `networks` — must BE arrays; the store spreads and reduces them with no guard at all |
| arrays | must be arrays **when present**; null and absent pass |

Null passes because a nil Go slice marshals to `null`, so a machine with no
GPU, no fans and no alerts firing sends `null` for all three. Presence is what
every call site already checks and it is not the same thing: `"x" != null` is
true, `"x".length > 0` is true, and then `.map()` throws.

**What this does not establish.** The validator checks the snapshot's *shape*
— that every section and list the page walks is the kind of thing it expects.
It does not descend into array elements, so it says nothing about the values
inside one, and that gap is not benign: `{"bytes_sent_rate": "x"}` passes,
coerces through arithmetic rather than throwing, and renders as a number or a
`NaN` while the page reports itself connected and current. The cases that do
throw only throw once the panel holding them is rendered, so a frame can be
presented as current from another tab. A delta can rewrite a single element, so
this is reachable frame by frame.

The guarantee is therefore narrower than "renderable": an accepted frame has
the **container shape** the page walks — the sections are objects and the lists
are lists. Whether it renders, and whether the readings inside those lists are
the right type, are both unchecked. Closing that means element-level validation, and
doing it by hand means restating the whole recursive `Snapshot` type in a second
dialect, a copy whose own drift refuses *every* frame and blanks the dashboard
the moment a field is added on the Go side and missed here. Generating the
schema from the Go types has neither problem and is tracked as **#38**.

`sensors.psi` is the one section allowed to be missing entirely — a kernel
without pressure-stall information does not report it and the page guards for
that — and it is not optional in halves: `psiRows` reads `some_avg10` off
whatever it is handed, so `cpu`, `memory` and `io` must all be there when
`psi` is.

A field the server adds is ignored rather than refused, so the tables move only
when something the UI already reads is renamed or removed — at which point the
client is broken either way and failing loudly is the better outcome.

**A connecting client is given the newest frame, and nothing older behind it.**
`subscribe()` starts queueing immediately, so between registering and reaching
its loop a handler can accumulate frames — and the cache holds the newest of
them. Sending the cache and then draining normally delivered seq 12 and then
seq 11, and a client renders a `full` frame whatever sequence it carries,
because it has to: a periodic resync arrives at an arbitrary one. So the
dashboard stepped *backwards* to an older reading and presented it as current.
The queue is therefore drained before the cache is read — safe in that order,
and only in that order, because `fanOut` stores the cache before it queues, so
nothing newer than the replacement can be discarded.

## When the monitor is gone

A stopped monitor still answers `Snapshot()` with the last thing it collected,
and still hands out subscriptions that nothing will ever write to or close. The
delta hub restarts on demand, so a client connecting after shutdown used to be
sent that cached reading as an ordinary `full` frame, marked current, followed
by keepalives for as long as the tab stayed open — a measurement from before the
service stopped, presented as live. **Both protocols on the endpoint make that check** — the delta path and the
legacy `snapshot` one, which had the same hole for as long as it has existed
and does not go through the hub at all, which is exactly why the hub's guard
did not cover it. A fresh run checks `Monitor.Running()`
after subscribing and closes its subscribers instead, so the stream ends and the
dashboard reconnects on its backoff showing the last reading labelled
disconnected.

The cached frame is the other half. A connecting client is served the last
published frame before anything else arrives, so that it is not blank between
ticks — and that cache outlives the run that produced it. It was cleared only
when the last subscriber left, so while one dashboard stayed attached after a
stop, a second one connecting alongside it was sent that pre-stop frame as a
`full`, rendered it, marked it current, and then received `bye`. The run now
clears the cache before it closes anyone, and the handler will not serve it
while the monitor is not collecting: the first closes the ordinary case, the
second closes the window between a run ending and the clear landing.

The cache is also stamped with the **monitor** run that produced it
(`Monitor.Generation()`), and refused when that is not the current one.
`Running()` alone is not enough: a stop followed by a start makes it true again
immediately, while the hub's publishing loop may not yet have noticed its
subscription closed, and the frame still sitting there is then from before the
gap — for a service restarted after an hour, an hour old. The hub's own run
counter does not work here either, because in that window its run has not
turned over. The generation has to come from the thing that actually restarted.

Stamping alone would still not be enough. A fresh run publishes
`Monitor.Snapshot()` as its first frame, so that a client connecting between
ticks is not blank — and after a restart that was the *previous* run's
collection, stamped with the new generation and served as current. `Start` now
discards it: a run that has collected nothing has no latest snapshot. The
reading is still readable while the monitor is stopped, which is the deliberate
half of that behaviour; it simply does not survive into the next run.

Finally, the generation travels **with the collection** rather than being read
when the frame is built. A stop and a start can complete between the hub
dequeuing a snapshot and publishing it, and stamping at that point would give a
measurement from the previous run the current run's number — which is exactly
the mark that says it can be trusted. A frame whose run has ended is dropped
instead. The sequence has already advanced, so every client is behind and the
next frame reaches them as a whole snapshot: a gap, which is always preferable
to a reading from before the restart.

The generation is read *before* the loop waits for a collection, not after one
arrives, so the comparison can only ever be conservative — a restart while the
loop is blocked makes the number stale and costs a tick, where reading it
afterwards would stamp an old measurement with the new run's number.

**What is not guaranteed on either protocol**, and cannot be: a frame that was
current when it was published can still reach a socket after the monitor has
stopped. Every check here is a check-then-write, and the two are separate
operations — the handler can find the subscription open, be descheduled, and
resume after the stop has completed. Closing that would mean synchronising a
network write with the monitor's shutdown, which lets one slow client hold up
the stop. The staleness there is
bounded by the write itself and is corrected immediately — the `bye` follows on
the same connection, and the dashboard marks itself disconnected. That is a
different thing from the defect this section describes, which was a reading
from a stop that had *already completed*, served to a client that then received
nothing but keepalives.

## Keeping the two implementations in step

`pkg/delta` and `cmd/sysmon/frontend/src/lib/delta.ts` apply the same format,
and nothing in either language connects them. A disagreement would not fail
anything: the server would go on sending correct patches and the dashboard would
go on rendering something subtly wrong, indefinitely.

Both are run against two committed fixtures, generated by the Go side and read
by `tests/delta.spec.ts`. A change to the format has to be made in both places,
or one of them fails.

| Fixture | What it holds |
|---|---|
| `pkg/delta/testdata/cases.json` | patches the differ emits, with the exact result |
| `pkg/delta/testdata/refusals.json` | patches that must be **refused** |
| `pkg/delta/testdata/accepted.json` | patches the differ never emits that a receiver must still handle, with the exact result |

```
go test ./pkg/delta -run TestFixture -update            # regenerate cases.json
go test ./pkg/delta -run TestRefusalFixture -update     # regenerate refusals.json
go test ./pkg/delta -run TestAcceptedFixture -update    # regenerate accepted.json
```

`cases.json` is generated from before/after pairs, so it can only hold what the
differ produces. The wire is not the differ: `accepted.json` is where a frame
some other sender could emit is pinned to one answer.

The refusal fixture was added after the two halves drifted in exactly the way
this section warns about. The browser refused a keyed patch naming an element
the array does not contain; Go applied it, returned the unchanged tree and
reported success — an element silently keeping a value the server had changed,
which is a stale reading rendered as current. Both sides were passing their own
tests, because the positive cases were shared and the refusals were written
twice by hand. Every case in `refusals.json` is proved to fail in Go before it
is written out, so the fixture can never ask the browser to reject something the
server accepts.

`pkg/delta` also has a property test: three thousand random trees, randomly
mutated, each of which must survive diff-encode-decode-apply unchanged. It is
what found the null-replacement bug above.
