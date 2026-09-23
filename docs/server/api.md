# API reference

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
    "version": "0.1.0-alpha",
    "git_commit": "a46f6cf",
    "build_date": "2026-09-05T03:26:50Z",
    "node_name": "db-01",
    "alerts": [ ... ]
}
```

`version` is the release string and `git_commit`/`build_date` are the build it
was cut from. All three are the same values `GET /api/version` reports, and the
last two are the only ones that identify a build: the release string has read
`0.1.0-alpha` on every build this project has produced, so it cannot tell two
apart. They were added in an additive change — a client that does not read them
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
    "version": "0.1.0-alpha",
    "uptime_seconds": 864000,
    "node_name": "ci-node-1",
    "collection_interval_ms": 1000,
    "git_commit": "a46f6cf",
    "build_date": "2026-09-22T12:00:00Z",
    "critical_count": 0,
    "warning_count": 2,
    "alerts": [ ... ],
    "cpu_percent": 43.7,
    "memory_percent": 73.7,
    "load_per_core": 0.36,
    "swap_percent": 51.1,
    "swap_counters_available": true,
    "swap_rate_available": true,
    "swap_in_rate": 0,
    "swap_out_rate": 0,
    "worst_disk_percent": 88.2,
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

The fleet view: `{enabled, nodes, rollup}`. `enabled` is false on a node with
no peers, and then there is no roll-up. Each node is a `NodeSummary` with the
poll's own fields beside it: `name`, `url`, `self`, `reachable`, `error`,
`age_seconds`, `status`, `build_match`, and `source` (`local`, `summary`, or
`snapshot` for a peer too old to serve a summary). `rollup` is the fleet-level
statement: state counts, how long the longest absence has lasted, the nodes
needing attention with their reasons, worst values with the node named, and
sums with their denominators. Every field is described in
[the fleet section](alerting-fleet-config.md#fleet).

## GET /api/fleet/{name}/snapshot

One node's full snapshot, fetched from it on demand, in an envelope:
`{snapshot, reachable, error, age_seconds}`. If the fetch fails and the node
has been read before, the last snapshot is served with `reachable: false`. The
client must show it as stale. `404` means an unknown node, or this node before
its first collection. `502` means a peer that cannot be read and never has
been, or that did not answer within the response's write budget. `503` means
a reload moved the node while its snapshot was being fetched; ask again.

## GET /

Serves the dashboard when frontend assets are embedded. Without them you get
`200 OK` and the plain text `go-sysmon is running`, which is enough for a
health check.

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
{ "interval_ms": 1000 }
```

## POST /api/interval

Changes the polling interval.

```bash
curl -X POST http://localhost:8080/api/interval \
     -H 'Content-Type: application/json' \
     -d '{"interval_ms": 500}'
```

Allowed values: 250, 500, 1000, 5000, 10000, 15000, 30000, 60000. Anything else
is `400 Bad Request` and the current interval is left alone. The response
echoes the interval now in force.

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
the interface, the GPU. The rest of a Prometheus label set is either constant
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
| 400  | Unknown metric, a node this dashboard does not serve, or a window it will not serve — an end at or before the start, more than 90 days, or a step needing over 11,000 points (counted inclusively: a sample at the start, at every step, and at the end). |
| 502  | Prometheus could not be reached, or refused the query. The dashboard reached go-sysmon; go-sysmon did not reach Prometheus. The detail is in the server log, not the response. |

## Origins

The event stream is an ordinary HTTP response, so it is subject to the browser's
same-origin policy: a page on another origin cannot read it without CORS headers,
and the server sends none. This is a deliberate difference from the WebSocket
endpoint it replaced, whose handshake bypassed the same-origin policy entirely.

That is not authentication. Anything that can route to the port and is not a
browser can still read everything. See the note on access control in
[server/architecture.md](architecture.md).

## TLS

The server speaks plain HTTP by default. Supply a certificate to serve HTTPS:

```bash
sysmon serve --tls-cert /etc/ssl/sysmon.crt --tls-key /etc/ssl/sysmon.key
```

Applications embedding go-sysmon as a library have more options, including
resolving the configuration per handshake. See
[server/architecture.md](architecture.md#tls).
