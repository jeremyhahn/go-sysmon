# API reference

Two ways in: poll `/api/snapshot`, or connect to `/api/events` and let the
server push. Both carry identical JSON. Use the event stream unless you have a
reason not to — polling re-encodes a snapshot the server already has.

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
there is one per polling interval.

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
