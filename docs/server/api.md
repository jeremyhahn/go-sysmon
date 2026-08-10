# API reference

Two ways in: poll `/api/snapshot`, or connect to `/ws` and let the server push.
Both carry identical JSON. Use the WebSocket unless you have a reason not to —
polling re-encodes a snapshot the server already has.

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

## GET /ws

Upgrades to a WebSocket. The server starts streaming as soon as the handshake
completes — there is nothing to subscribe to and no hello message.

### What the server sends

Every message is a JSON-encoded `types.Snapshot`, one per polling interval,
same schema as `/api/snapshot`.

### What you can send

One command:

```json
{
    "type": "set_interval",
    "interval_ms": 500
}
```

Allowed: 250, 500, 1000, 5000, 10000, 15000, 30000, 60000. Anything else is
ignored silently — you will keep receiving snapshots at the previous rate.

Two things to know. The interval is shared by every connected client, because
there is one poll loop behind all of them. And 250ms exists but is rarely worth
it: most sub-collectors cannot produce genuinely new data that fast.

### Connection lifecycle

1. Client connects to `/ws`.
2. Server upgrades and begins streaming.
3. Client optionally sends `set_interval`.
4. Server pings every 30s; a client that has not ponged within 60s is dropped.
5. On monitor shutdown, the server sends a close frame with code 1000.

Handle step 5 by reconnecting. The Cinnamon applet waits three seconds and
retries, which is a reasonable default.

### Origins

`CheckOrigin` returns true for everything, so the server works behind a reverse
proxy and during local development without configuration. It also means any web
page you visit can read this server if it can reach it. See the note on access
control in [server/architecture.md](architecture.md).
