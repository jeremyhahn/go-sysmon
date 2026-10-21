# API reference

> **Status: NORMATIVE** · Scope: the go-sysmon HTTP API · Last verified: 2026-10-11
> Conflicts with this document are defects. Fix them here first.

Two ways in: poll `/api/snapshot`, or connect to `/api/events` and let the
server push. Both carry identical JSON. Use the event stream unless you have a
reason not to — polling re-encodes a snapshot the server already has.

**Lists are never `null`.** Every array field in every JSON response and
stream frame is `[]` when there is nothing in it — a host with no GPUs sends
`"gpus": []`, an NPU with no engines `"engines": []`, a standalone node's fleet
view `"nodes": []`. An empty list and "none" mean the same thing; the
distinction is deliberately not made (#151). See
[the types reference](../types/reference.md#lists-are-never-null) for how this
is enforced.

## GET /api/snapshot

The most recent snapshot, as JSON.

`200 OK`, `Content-Type: application/json`:

```json
{
    "timestamp": "2026-03-31T12:00:00Z",
    "host": { ... },
    "cpu_summary": { ... },
    "cpus": [ ... ],
    "gpus": [ ... ],
    "memory": { ... },
    "disks": [ ... ],
    "networks": [ ... ],
    "load_avg": { ... },
    "processes": { ... },
    "sensors": { ... },
    "virt": { ... },
    "version": "0.2.0-alpha",
    "git_commit": "a46f6cf",
    "build_date": "2026-09-05T03:26:50Z",
    "node_name": "db-01",
    "alerts": [ ... ]
}
```

`version` is the release string and `git_commit`/`build_date` are the build it
was cut from. All three are the same values `GET /api/version` reports, and the
last two are the only ones that identify a build: the release string changes
only when a release is cut, so every build between two releases carries the
same one and it cannot tell them apart. They were added in an additive change — a client that does not read them
is unaffected — and they are omitted entirely by a node that does not state its
build, which is what a binary built without the release `-ldflags` does. An
absent commit means the node said nothing, never that it agrees with yours.

`node_name` is the name this node writes its history under — the Prometheus
`instance` label, which is its `fleet.local_name` or, unset, its hostname. An
aggregator compares it with the name it holds for the node and reports the
result as `name_match` in `GET /api/fleet` (#162). A reload of
`fleet.local_name` restamps the published snapshot, so every path reports the
new name at once, as the exporter's label does. It is additive and omitted by a
node that predates it.

These fields, and `alerts`, are the same on every path that serves a snapshot,
whether the server is `sysmon serve` or embedded as a library; see
[architecture.md](architecture.md#every-path-serves-the-same-decorated-snapshot).

`503 Service Unavailable` means there is no snapshot to serve. The status is
the same for both reasons a client can see it — a client can act on neither
differently, so it retries either way — but the plain-text body now names
which one it is:

| Body                                       | What it means                                                              |
| ------------------------------------------ | -------------------------------------------------------------------------- |
| `monitor has not collected a snapshot yet` | The poll loop is running and its first collection has not finished. Ordinary on the way up, and it will not be long. |
| `monitor is not running`                   | Nothing is collecting and nothing has been collected.                       |

Both used to read `monitor is not running`, which named the rarer of the two
causes and sent readers after a lifecycle bug during an ordinary warm-up
(#33). The status code, the schema and every field are unchanged; only the
text of the 503 body is new, and it was never a value to branch on.

Field-by-field detail is in the [types reference](../types/reference.md).

## GET /api/summary

This node's `NodeSummary`: its own figures for a fleet view, about a kilobyte.
Every node serves it, because any node can be a peer. An aggregator polls it
instead of `/api/snapshot` (#76).

`200 OK`, `Content-Type: application/json`:

```json
{
    "hostname": "ci-node-1",
    "version": "0.2.0-alpha",
    "uptime_seconds": 864000,
    "node_name": "ci-node-1",
    "collection_interval_ms": 1000,
    "git_commit": "a46f6cf",
    "build_date": "2026-09-22T12:00:00Z",
    "critical_count": 0,
    "warning_count": 2,
    "alerts": [ ... ],
    "cpu_percent": 43.7,
    "cpu_available": true,
    "memory_percent": 73.7,
    "load_per_core": 0.36,
    "cpu_count": 112,
    "load_1": 40.3,
    "load_5": 38.9,
    "load_15": 35.2,
    "memory_total_bytes": 540949086208,
    "memory_available_bytes": 142270808064,
    "swap_percent": 51.1,
    "swap_counters_available": true,
    "swap_rate_available": true,
    "swap_in_rate": 0,
    "swap_out_rate": 0,
    "worst_disk_percent": 88.2,
    "disk_free_bytes": 1288490188800,
    "disk_total_bytes": 3840755982336,
    "disk_free_path": "/var/lib/docker",
    "containers_observable": true,
    "containers_running": 6,
    "container_cpu_cores": 21.4,
    "container_memory_bytes": 68719476736,
    "raid_arrays": 2,
    "raid_unhealthy": 0,
    "raid_degraded": false,
    "smart_capable": 10,
    "smart_unhealthy": 0,
    "max_temp_c": 80,
    "max_temp_source": "cpu",
    "power_w": 315,
    "power_reported": true,
    "net_rx_rate": 31457280,
    "net_tx_rate": 15728640
}
```

- `collection_interval_ms` is the interval the node's monitor actually collects
  at, the cadence its snapshot timestamps advance by. `0` means unknown: an
  older peer that does not report it, or a peer the aggregator reads by its
  whole snapshot. It is never assumed.
- `node_name` is the name the node writes its history under, the same value
  as in its snapshot. An aggregator compares it with the name it holds for the
  node and reports `name_match` in `/api/fleet` (#162). Omitted by a node that
  predates it.
- `alerts` holds only the warning and critical alerts, the ones that set the
  node's state.
- `cpu_percent` is the mean over the node's cores whose usage was measured.
  `cpu_available` is false when none was: the node's first collection, an
  unreadable `/proc/stat`, or a peer too old to report `usage_available`. The
  figure is then unknown, not zero, and the dashboard shows a dash.
- `swap_in_rate` and `swap_out_rate` are bytes per second paged over the
  node's last collection interval, from its two latest readings (#134).
  `swap_rate_available` is false when the rate could not be measured:
  unreadable counters, no previous reading yet, or counters that went
  backwards. The rates are then unknown, not zero.
- `smart_capable` includes the drives behind a hardware RAID controller, each
  counted once by serial even when several virtual disks list it.
- `max_temp_c` is the hottest of every component that reports a temperature
  (CPU cores, drives, GPUs, NPUs), and `max_temp_source` names which kind.
- `net_rx_rate` and `net_tx_rate` are summed over the node's physical network
  devices, so a bond, a bridge, a VLAN, a tunnel or a guest's vnet port, which
  carry traffic the devices also carry, add nothing twice. `net_reported` is
  false for a node with no physical device (a container in its own network
  namespace), and while any of its devices has no measured rate yet
  (`rates_available`). Its rates are then not measured, rather than zero.
- `power_reported` is false when the node has no power sensor, or when its
  sensors have not produced a reading yet (a CPU package needs two energy
  samples). Its `power_w`
  of 0 then means nothing.
- The capacity figures are absolute, where the percentages are relative: what
  a scheduler needs to place work (#239). `cpu_count` is logical cores and
  `load_1`/`load_5`/`load_15` the raw load averages. `memory_total_bytes` and
  `memory_available_bytes` are RAM and the part available without swapping.
  `cpu_count` or `memory_total_bytes` of 0 is unknown, and so are the loads
  beside a `cpu_count` of 0.
- `disk_free_bytes` and `disk_total_bytes` are the filesystem holding the
  container runtime's data root, or `/` on a node with no runtime;
  `disk_free_path` names the path measured (the snapshot's
  `virtualization.runtime_disk`). **An empty `disk_free_path` is unknown**, never
  a full disk. `worst_disk_percent` is unchanged and may describe another
  filesystem.
- `containers_running`, `container_cpu_cores` (summed percent-of-one-core, so
  `2` is two cores) and `container_memory_bytes` cover the containers with
  processes in their cgroups. They come from the cgroup tree, not the runtime
  API, which is usually unreachable. They mean nothing when
  `containers_observable` is false: the cgroup tree could not be read in full.
  A container first seen in the node's latest scan contributes no CPU until the
  next one.
- A node on a build that predates any of these fields omits them, and a
  consumer decoding into this type reads them as the unknown forms above.

`503 Service Unavailable` before the first collection, with the same bodies as
`/api/snapshot`.

## GET /api/snapshot/history

The latest snapshot without its process list and per-CPU `flags`. Both are
sent as `[]`. An aggregator that records the fleet's history fetches this from
each peer at its sample interval, from a worker separate from the summary
poll, and only while it records. Nothing history
records is removed: the process counts stay, and a test checks that every
field `metrics.Write` reads survives the slimming. On a busy host this is about
a fifth of the whole snapshot.

`503 Service Unavailable` before the first collection.

## GET /api/fleet

The fleet view: `{enabled, nodes, page, self, rollup}`. `enabled` is false on a
node with no peers, and then there is no page, no `self` and no roll-up.

`nodes` is **one page** of the fleet, in name order (#15):

| Parameter | Meaning | Absent |
|---|---|---|
| `q` | keep nodes whose `name` or `hostname` contains it, A–Z case-folded; surrounding space is trimmed | every node |
| `offset` | skip that many matching nodes; an integer ≥ 0 | `0` |
| `limit` | at most that many nodes; an integer ≥ 1 | no limit |
| `anchor` | with `limit`: open the page holding the node of this exact name instead of `offset`; `page.offset` says where that is. Ignored if no matching node has the name | `offset` applies |

`page` describes the answer: `{q, offset, limit, matched, total}` — the query
as applied, how many nodes matched it across every page, and how many the
fleet has. An offset past the last match is an empty page, not an error: a
node leaving the fleet can shrink the list under a client already on its last
page, and `matched` tells it where the end now is. A malformed parameter —
not an integer, below its minimum, given twice, or a `q` that is not UTF-8 —
is `400 Bad Request` naming the parameter, on a standalone node too. So is a
query string that does not parse at all (`limit=25;offset=0`): it is refused
whole rather than having the bad pair dropped and the rest answered unbounded.

The order is the node order in `pkg/fleet/order.go`, a total order, so pages
are contiguous runs of one list: walking them by `offset` visits every
matching node exactly once. With no parameters the response is the whole
fleet, as before paging existed. The dashboard always asks for a page: at ten
thousand busy nodes the whole list is about 9 MB, a page of 25 about 25 KB.

`self` is the node serving the view, whatever the page — the build every
node's `build_match` was compared with.

Each node is a `NodeSummary` with the poll's own fields beside it: `name`,
`self`, `reachable`, `error`, `age_seconds`, `status`, `build_match`,
`name_match`, and `source` (`local`, `summary`, or `snapshot` for a peer too
old to serve a summary). While the aggregator records history, each peer also
carries `history`: `{state, reason}`, where `state` is `recorded`, `refused`
(the peer collects too slowly for its history to be charted, and none is
written) or `unverified` (written, but the peer reports no collection
interval, or the database's lookback is not known yet); the card shows the
last two. `reason` is worded from durations alone and names no address. It is
carried on every page a node appears on.

A reachable node also carries `headroom` (#239): `{target, cpu_cores_free_at_target,
cpu_reported, memory_bytes_free, memory_reported}`, its spare capacity at this
aggregator's `fleet.headroom_target`. It is computed here, not by the node,
because the target is the aggregator's policy, and a subtree node is restated at
this aggregator's target. `headroom` is absent for an unreachable node and for
one where neither half could be computed; each half's `*_reported` says whether
it was. See [Capacity and headroom](alerting-fleet-config.md#capacity-and-headroom).

**No field carries a peer's URL or address** (#120). The endpoint has no
authentication of its own, and a peer's URL is its scheme, address and port on
the internal network. A node is addressed by `name` everywhere, including
`/api/fleet/{name}/...`; the URL stays in the aggregator's configuration and
poller. A peer's `error` is worded without the address too: `fleet: request to
peer fs failed: connection refused`, not the transport's `Get
"http://198.51.100.10:8081/api/summary": dial tcp …`. The reasons are a fixed
set — `connection refused` and the kernel's other socket errors, `timed out`,
`host name not found`, `certificate signed by an unknown authority`, `peer
refused the TLS handshake: tls: certificate required`, `peer did not answer
with TLS`, `malformed response: …` — and anything else reads `request failed;
see this node's log`. The aggregator logs the whole error at debug level, on
its own host. The roll-up's `attention` reasons are the same strings.

`rollup` is the fleet-level statement, **always over the whole fleet and never
over the page**, whatever `q`, `offset` and `limit` say: state counts, how long
the longest absence has lasted, the nodes needing attention with their
reasons, worst values with the node named, and sums with their denominators.
With any of `q`, `offset` or `limit` its per-node lists are bounded — at most
five nodes needing attention, five build groups and five names in each — so
its size does not grow with the fleet; the counts beside them are whole. A
request with no parameters is already the size of the fleet and gets the
lists whole, which is also what a consumer written before paging reads. Every field is described in
[the fleet section](alerting-fleet-config.md#fleet).

On an aggregator with child aggregators (#190) the view is the whole tree. A
node of a child's subtree carries `via`, the child it is reached through; the
roll-up adds `subtree_errors` (children whose subtrees are not composed) and
`name_conflicts` (nodes sharing a name with another), and a node may carry
`name_conflict`, `subtree_error` and `aggregates`. Search, order, counts and
paging run over the index each child is polled for, and only a page's subtree
nodes are fetched from their children on request, reused for the poll
interval. A request with no parameters fetches nothing from the children: its
subtree nodes, and any beyond the 250 a view fetches, carry `source: "index"`,
with their state known and their figures absent. See
[Hierarchical aggregation](alerting-fleet-config.md#hierarchical-aggregation).

## GET /api/fleet/rollup

The same whole-fleet roll-up alone, bounded as a page's is, for a consumer that wants the fleet's
state and none of its nodes. `404` on a node with no peers: there is no fleet
to roll up.

## GET /api/fleet/subtree

What a parent aggregator polls from this one each cycle (#190):
`{nodes, rollup}`. `nodes` is an index of every node in this fleet, this node
included: `{name, hostname, status, reachable, age_seconds, depth,
name_conflict, reasons, commit}`, with `depth` 0 for this node and 1 for its
own peers. `rollup`
is the roll-up in the form a parent composes: every build group is kept, each
naming at most five nodes, so groups sum exactly across children. `404` on a
node with no peers: it is a leaf, and its summary's `aggregates` is 0.

## GET /api/fleet/nodes

`?name=a&name=b…`: the named nodes of this fleet, whole, as `{nodes}` — what a
parent aggregator fetches to fill a page with nodes of this one's subtree. A
name not in the fleet, or in conflict, is omitted. Each name must be a valid
node name, one to 1000 of them; anything else is `400`. `404` with no fleet.

### Between aggregators

Every request one aggregator makes of another carries
`X-Sysmon-Fleet-Ancestry` (the instance IDs of the aggregators it has passed
through) and `X-Sysmon-Fleet-Budget-Ms` (the time the sender will still wait).
`/api/fleet`, `/api/fleet/subtree`, `/api/fleet/nodes`,
`/api/fleet/{name}/snapshot`, `/api/fleet/{name}/config` and a relayed
`/api/history` all read them. A chain containing this aggregator's own ID is
refused with `508 Loop Detected` and `X-Sysmon-Fleet-Refusal: loop`. A chain
longer than four is refused with `508` and `depth`. A malformed chain is
`400`. The budget only ever shortens this node's own, and is honoured only
with a valid chain; a browser's is ignored. A relayed history range is
decoded and re-served as this node's JSON; a child's error comes back as its
status (400, 404 or 409, otherwise 502) and never its body. See
[Hierarchical aggregation](alerting-fleet-config.md#hierarchical-aggregation).

## GET /api/fleet/{name}/snapshot

One node's full snapshot, fetched from it on demand, in an envelope:
`{snapshot, reachable, error, age_seconds}`. If the fetch fails and the node
has been read before, the last snapshot is served with `reachable: false`. The
client must show it as stale. `404` means an unknown node, or this node before
its first collection. `502` means a peer that cannot be read and never has
been, or that did not answer within the response's write budget. `503` means
a reload moved the node while its snapshot was being fetched; ask again. An
error body names the node, never its address, as in `GET /api/fleet`. For a
node of a child aggregator's subtree (#190) the drill-in is routed to that
child, and its envelope comes back as the child built it. `409` means two
nodes in the fleet tree carry the name, and neither is routed to.

## GET /api/fleet/{name}/config

One node's settings view, the same `{config_file, entries}` shape as
`GET /api/config`. For this node's own name it is exactly `GET /api/config`.
For a peer the aggregator fetches the peer's `GET /api/config` and relays it
with the peer's topology withheld: `server.addr`, the peer's bind address, is
reported as `(configured)`, and so is anything an older build still sends
verbatim — certificate paths, Prometheus addresses, the config file's path,
peer URLs. A peer entry keyed by a URL (a build older than #37) is re-keyed
`fleet.peers.(unnamed N)`. See
[What the settings view withholds](alerting-fleet-config.md#what-the-settings-view-withholds).
`502` means the peer could not be read; the body names it without its address.
A subtree node's settings are routed to its child aggregator and withheld again
here; `404` is a name not in the fleet, `409` a name in conflict.

## GET /

Serves the dashboard when frontend assets are embedded. Without them you get
`200 OK` and the plain text `go-sysmon is running`. It says nothing about the
collector; probe `/readyz` instead.

## GET /healthz

Liveness. `200 OK` and `ok` for as long as the process serves requests. It
checks nothing else, so use it only where a failure should restart the
process.

## GET /readyz

Readiness. `200 OK` and `ready` when both hold:

- the latest snapshot is no older than **three stream intervals, and never
  less than 10s** — 10s at 500ms or 1s, 15s at 5s, 180s at 60s. The bound
  follows `POST /api/interval` immediately;
- the server is not draining (`Stop` has not begun).

Otherwise `503 Service Unavailable` with a one-line reason, for example
`not ready: latest snapshot is 42s old, more than the 15s allowed`,
`not ready: server is draining`, or the monitor's own error before the first
snapshot. The container image's `HEALTHCHECK` and `docker-compose.yml` probe
this route.

Both probes are plain text with `Cache-Control: no-store`, answer `HEAD`, and
sit beside the other routes on the server's handler, so an application that
mounts go-sysmon under a prefix finds them under that prefix.

## GET /api/events

A [server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
stream. `Content-Type: text/event-stream`. The server starts sending as soon as
the response headers are out — there is nothing to subscribe to and no hello
message.

In a browser:

```js
const events = new EventSource("/api/events");
events.addEventListener("snapshot", (ev) => {
    const snapshot = JSON.parse(ev.data);
});
```

From a shell:

```bash
curl -N -H 'Accept: text/event-stream' http://localhost:8080/api/events
```

### What the server sends

| Frame              | Meaning                                                  |
|--------------------|----------------------------------------------------------|
| `retry: 3000`      | Reconnect delay hint, sent once before anything else      |
| `event: snapshot`  | A JSON `types.Snapshot`, same schema as `/api/snapshot`   |
| `event: bye`       | The monitor stopped; stop reconnecting                    |
| `: keepalive`      | A comment every 30s so idle connections are not reaped    |

The first `snapshot` is the server's cached one, delivered immediately rather
than at the next poll tick, so a reconnecting client is never blank. After that
there is one per polling interval, plus one whenever a reload renames the node:
that frame repeats the current reading — same `timestamp`, same values — with
the new `node_name`. A client that keeps history should add one point per
`timestamp`, not one per frame.

### Compression

The stream is gzip-encoded when the client sends `Accept-Encoding: gzip`. This
matters more here than for a normal response: consecutive snapshots are nearly
identical, and the deflate dictionary carries across flushes, so every snapshot
after the first compresses against the one before it. A client that cannot
inflate should send `Accept-Encoding: identity`.

### Connection lifecycle

1. Client opens `GET /api/events`.
2. Server sends `retry:`, then the cached snapshot, then one per interval.
3. Server writes a keepalive comment every 30s over an idle stream.
4. On monitor shutdown the server sends `event: bye` and closes.

`EventSource` reconnects on its own using the advertised `retry` value, so step
4 is the only case a client has to handle explicitly: a `bye` means stop
retrying. Non-browser clients should implement their own reconnect with
backoff; the Cinnamon applet doubles its delay up to 30s and resets it once
data arrives.

### Delta mode

`GET /api/events?deltas=1` sends what changed instead of a whole snapshot: a
`full` frame on connection and after any gap, `delta` frames in between, each
carrying a monotonic `seq`. It is a saving of roughly 4–7x on the wire.

Without the parameter the stream is exactly what it was — `snapshot` events
carrying the bare snapshot — so an existing client needs no change.

The format, the resync rules and the measurements are in
[delta-streaming.md](delta-streaming.md).

## GET /api/interval

The monitor's current polling interval.

`200 OK`, `Content-Type: application/json`:

```json
{ "interval_ms": 1000, "fleet_interval_ms": 1000 }
```

`fleet_interval_ms` is the cadence peers are refreshed at, after the floor and
the configuration are resolved. It is omitted when the server has no fleet
poller, which is only the case for an application embedding `pkg/server`
without one: `sysmon serve` always runs one, peers or not.

## POST /api/interval

Changes the polling interval.

```bash
curl -X POST http://localhost:8080/api/interval \
     -H 'Content-Type: application/json' \
     -d '{"interval_ms": 500}'
```

Allowed values: 250, 500, 1000, 5000, 10000, 15000, 30000, 60000. Anything else
is `400 Bad Request` and the current interval is left alone, as is a body that
is not JSON or is over 1 KiB. The response echoes the intervals now in force.

`scope` says which poller the value is for. Empty or `"local"` is this node's
own monitor. `"fleet"` is the peer refresh cadence, which is floored at one
second: a request for 250ms is answered `"fleet_interval_ms": 1000`, the
cadence actually in force. `"fleet"` on a server with no fleet poller, and any
other scope, is `400`.

```bash
curl -X POST http://localhost:8080/api/interval \
     -H 'Content-Type: application/json' \
     -d '{"interval_ms": 5000, "scope": "fleet"}'
```

Two things to know. The interval is shared by every connected client, because
there is one poll loop behind all of them — this endpoint changes the rate for
every browser tab and every applet at once. And 250ms exists but is rarely worth
it: most sub-collectors cannot produce genuinely new data that fast.

## GET /api/history/metrics

Lists the metrics history can be drawn for.

```json
{
  "enabled": true,
  "metrics": [
    {"key": "cpu", "label": "CPU", "unit": "percent", "split_by": ""},
    {"key": "disk_used", "label": "Filesystem used", "unit": "percent", "split_by": "device"}
  ]
}
```

`enabled: false` means no `prometheus.query_url` is configured. It is not a 404:
a standalone node and a build without the endpoint would then be
indistinguishable, and the dashboard has to tell them apart to know whether to
hide the date picker or report a failure.

`key` is not the metric name. Several keys share a metric and differ only by a
label matcher — `psi_cpu` and `psi_memory` are both `sysmon_psi_full_avg10`.

`unit` is one of `percent`, `bytes_per_second`, `celsius`, `watts` or
`count`. A counter is always drawn in `bytes_per_second`, as a rate.

### Counters

Most entries are gauges, queried as a plain selector. A few are **counters** —
`swap_in` and `swap_out`, over `sysmon_swap_in_pages_total` and
`sysmon_swap_out_pages_total` — and those are queried as a rate, never as the
running total, and drawn in **bytes per second**:

```
rate(sysmon_swap_out_pages_total{instance="fs"}[120s])
  * sysmon_memory_page_size_bytes{instance="fs"}
  and sysmon_swap_out_pages_total{instance="fs"}
```

A cumulative total drawn over time is a line that only rises, with a cliff at
every reboot; `rate()` turns it into how much is happening now and treats the
reboot as a counter reset rather than a drop.

**Bytes, from each node's own page size.** The kernel counts pages, and a page
is 4 KiB on x86 but 16 or 64 KiB on some arm64 kernels, so every node exports
its own as `sysmon_memory_page_size_bytes` and the rate is multiplied by it. A
node that does not export it — a peer running a build from before it existed —
matches nothing, and its swap charts are empty rather than scaled by a guess.
The two are matched on every label, not `on(instance)`: the gauge is written
with exactly the counter's labels, and joining on instance alone turns
many-to-many — and fails the query — while old and new series coexist after an
`external_labels` reload.

**Only while the counter is there.** A range selector ignores Prometheus'
lookback, so a week's window would keep answering with the last rate long after
a counter stopped being written — and an unreadable counter is omitted on
purpose, to leave a gap. The trailing `and <counter>` is an instant selector,
which does apply the lookback, so the gap stays a gap.

**The window.** A counter is read in two steps. First the server measures the
longest gap between the counter's stored samples, then it takes the rate over

```
window = max(step + gap, 2 * gap)
```

`step + gap` makes consecutive windows overlap by a sample, so an increase
between two chart points is counted in one of them instead of falling between
both. `2 * gap` is what guarantees the two samples `rate()` needs: Prometheus
windows are left-open, `(t-w, t]`, and with no two samples more than `gap`
apart the newest sample at or before `t` is later than `t - gap` and the one
before it later than `t - 2*gap`. A whole number of seconds is written as `Ns`,
anything else as milliseconds, rounded up.

**The gap is measured, not configured.** Samples are written once per real
collection, at the collection's time (#173), so how far apart they are depends
on who wrote them and how often that node was collecting — this node's
exporter, the fleet poller relaying a peer, a peer's own exporter, a dashboard
interval changed halfway through the range — none of which the node drawing the
chart can know. So it asks the data: it reads `timestamp(<counter>)` over the
range at the finest resolution Prometheus will serve for it, which at each
point is the time of the newest sample, and takes the largest difference
between adjacent points. That difference is never smaller than the real gap it
spans: the last point before a sample `b` sees one no later than the sample
`a` before it, and the next point sees `b` or later. Two things widen it:

- **Before the range.** The window at the first point reads back before
  `start`, so the measurement starts earlier — by the step plus two lookbacks —
  and reaches back further, measuring again, if the window it finds is wider
  than that.
- **At the live edge.** The newest sample is as old as it is, and the one after
  it is not written yet: that open gap is counted too, one millisecond longer
  than it has lasted — it is strictly longer, since nothing landed at `end`,
  and a left-open window of exactly twice it would drop the sample before the
  newest. The measurement is aligned so an evaluation lands exactly on `end`,
  and the open gap counts only for a series present there with two samples
  since its last hole: one already past the lookback at `end` has stopped, and
  a lone sample after an outage has no cadence yet — counting its age would
  reach a window back across the outage.

A hole longer than the database's query lookback — which shows as a point with
no value — is the node being absent —
down, or not recording — which is how the database treats it for every chart,
gauges included. A node writing history is refused any cadence slower than that
(see the Prometheus section of the configuration guide), so its regular gaps are
always shorter. An outage is not measured across — sizing every window in the
chart from it would smear the whole range flat — though a window still spans a
hole shorter than itself, as any `rate()` does: the samples either side are
real, and the increase between them is the average of what happened. A jump in
sample time between two points that both have a value is always measured as a
gap: it can be healthy samples the evaluations stepped over (samples at 0s,
299s and 301s show as a 301s jump), so it cannot prove an outage, and counting
it can only widen the window, never empty it. A range
with no two samples close enough to measure has no rate to draw, and is
answered as an empty range.

**The lookback is read from the database**, not assumed, from
`GET /api/v1/status/flags` (`query.lookback-delta`), and cached for ten minutes
(thirty seconds after a failure to reach it). What each backend answers:

| Backend | Answers | Used |
|---|---|---|
| Prometheus | `query.lookback-delta` | as reported |
| Thanos | `query.lookback-delta` (every component, v0.15 on) | as reported |
| either, reporting `0s` | the PromQL engine replaces a zero with its default | 5m — the engine's `defaultLookbackDelta`, and Thanos' documented "promql default" |
| Grafana Mimir | an empty flag map, by design; `buildinfo` names it | 5m — Mimir's documented `-querier.lookback-delta` default, which it does not publish; a changed value is not visible |
| VictoriaMetrics | 404; it has no fixed lookback (it follows the median interval between samples) | none |
| anything else without the flag | — | none |

With none, counter history is refused with a 502 that says so —
`counter history needs the time-series database's query lookback, which it does
not publish` — rather than drawn with a lookback the database never gave.
Gauges are unaffected.

A range Prometheus would refuse — more than 11,000 points — is refused before
the measurement, exactly as for a gauge. The measurement reads before `start`
as the rate query does — each by its window, and by one database lookback more
for the instant selectors in them (`timestamp()`, and the presence gate) — so
the 90-day
bound on one request counts both: a counter's range **plus** what either reads
before it must fit in 90 days. Without that, an hour at a one-year step would be
one point on the chart and a year of samples read by Prometheus.

## GET /api/history

Answers one metric over one date range, for one node.

| Parameter | Meaning |
|-----------|---------|
| `metric`  | A key from `/api/history/metrics`. Required; anything else is a 400. |
| `node`    | The node to read, matching its fleet name. Defaults to this node. Must be this node or one of its configured peers. |
| `start`   | Unix milliseconds. Defaults to an hour before `end`. |
| `end`     | Unix milliseconds. Defaults to now. |
| `step`    | Milliseconds. Defaults to a resolution chosen for the window. |

```json
{
  "enabled": true,
  "node": "fs",
  "metric": "disk_used",
  "label": "Filesystem used",
  "unit": "percent",
  "start_ms": 1788300000000,
  "end_ms": 1788303600000,
  "step_ms": 6000,
  "series": [{"name": "sda", "points": [{"t": 1788300000000, "v": 41.2}]}]
}
```

`step_ms` is echoed because the server may have chosen it: a chart that does not
know the step cannot tell a gap in the data from the space between samples.

Series carry only the label that separates them from each other — the device,
the interface, the GPU. A CPU core is named `package:core` (`"1:0"`), because
every package numbers its cores from 0; a series written before cores carried
their package cannot say which package it was, and is not returned. The rest of a Prometheus label set is either constant
across the response or the operator's own labelling, and forwarding it would
publish the fleet's Prometheus configuration to any browser that can reach the
dashboard.

**The query is built from a fixed catalog, never from the request.** This service
has no authentication of its own; forwarding an expression would make it an open
PromQL proxy for the whole time-series database. The node name is validated
against the node-name rule (`pkg/nodename`) and escaped as well, **and** checked against this
node's name and its configured peers: a whole fleet writes into one Prometheus,
so a merely well-formed name would let anyone reaching one dashboard read the
history of every machine in the database.

A 502 says only that Prometheus could not be read. The address of the fleet's
time-series database, and whatever it replied, stay in the server log — the same
reason the settings view redacts the URL.

Status codes:

| Code | Meaning |
|------|---------|
| 200  | Answered. An empty `series` is a real answer: the node has no data for that window. |
| 400  | Unknown metric, a node this dashboard does not serve, or a window it will not serve — an end at or before the start, more than 90 days, a step needing over 11,000 points (counted inclusively: a sample at the start, at every step, and at the end), or, for a counter, a range plus rate window longer than 90 days. |
| 502  | Prometheus could not be reached, or refused the query. The dashboard reached go-sysmon; go-sysmon did not reach Prometheus. The detail is in the server log, not the response. |

## Origins

The event stream is an ordinary HTTP response, so it is subject to the browser's
same-origin policy: a page on another origin cannot read it without CORS headers,
and the server sends none. This is a deliberate difference from the WebSocket
endpoint it replaced, whose handshake bypassed the same-origin policy entirely.

That is not authentication. Anything that can route to the port and is not a
browser can still read everything. See the note on access control in
[server/architecture.md](architecture.md).

## Security headers

Every response carries the same browser security headers, whatever answers
it: JSON, the event stream, `/metrics`, the dashboard's files, the status page,
and the server's own 404 and 405 answers.

| Header | Value |
|--------|-------|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` |
| `Permissions-Policy` | every powerful feature denied, for example `camera=(), microphone=(), geolocation=()` |
| `Strict-Transport-Security` | `max-age=63072000`, **only on a request that arrived over TLS** |

`nosniff` means a browser takes each response as the type it declares and never
guesses, so a JSON body can never be run as a script or rendered as a page. The
policy allows no inline script or inline style, and `frame-ancestors 'none'`
stops any other page from framing the dashboard.

HSTS is sent when the connection is TLS, whether `--tls-cert`/`--tls-key`,
a `GetConfigForClient` hook, or an embedding application terminated it. It is
not sent over plain HTTP, because browsers ignore it there (RFC 6797 §8.1).
Behind a reverse proxy that terminates TLS, the proxy has to send it: the
request reaching go-sysmon is plain HTTP.

A dashboard framed inside another application, or served under a different
policy, changes these through `Config.SecurityHeaders`; see
[server/architecture.md](architecture.md#security-headers).

## TLS

The server speaks plain HTTP by default. Supply a certificate to serve HTTPS:

```bash
sysmon serve --tls-cert /etc/ssl/sysmon.crt --tls-key /etc/ssl/sysmon.key
```

Applications embedding go-sysmon as a library have more options, including
resolving the configuration per handshake. See
[server/architecture.md](architecture.md#tls).
