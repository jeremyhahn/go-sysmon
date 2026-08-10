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
    "virt": { ... }
}
```

`503 Service Unavailable` means the monitor has not finished its first
collection yet. Retry; it will not be long.

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
