# Server architecture

`pkg/server` puts the metrics on HTTP and WebSocket. It collects nothing itself
— every response comes from the monitor.

## Endpoints

| Method | Path            | What it does                                        |
|--------|-----------------|-----------------------------------------------------|
| GET    | `/`             | The dashboard, or a plain status line when no assets are embedded |
| GET    | `/api/snapshot` | The latest snapshot as JSON                         |
| GET    | `/ws`           | Upgrades to a WebSocket and streams                 |

## The WebSocket protocol

Connect to `ws://host:port/ws` and the server starts sending immediately, at
the monitor's current interval. Each message is a JSON-encoded
`types.Snapshot` — the same shape `/api/snapshot` returns.

To change the rate, send:

```json
{
    "type": "set_interval",
    "interval_ms": 1000
}
```

Accepted values are 250, 500, 1000, 5000, 10000, 15000, 30000 and 60000. The
server checks against that allowlist and ignores anything else rather than
erroring, so a client sending garbage keeps streaming at whatever rate it had.

Note that the interval is global. A client that changes it changes it for
everyone connected, because there is one poll loop behind all of them.

### Keep-alive

Ping frames every 30 seconds; a client that has not ponged within 60 gets
disconnected. Writes time out after 10. Without this, a client that vanished
without closing its TCP connection would hold a subscriber slot indefinitely.

### Shutdown

When the monitor stops, the server sends a normal close frame with the reason
"monitor stopped". Streaming clients get a clean disconnect rather than a
truncated read, which matters because `http.Server.Shutdown` does not track
hijacked WebSocket connections and will not wait for them.

## Static assets

The dashboard is served from an embedded filesystem. Two cache policies:

- `/assets/*` is content-hashed by the bundler, so it is `immutable` with a
  long max-age.
- `index.html` is `no-cache`. It has to be revalidated on every load, because
  it is the file that names the current bundle. Serving it without a validator
  pins a browser to whichever build it saw first, which makes every subsequent
  deploy invisible. That bug is easy to ship and very confusing to diagnose.

## Constructing one

```go
func New(mon *monitor.Monitor, addr string, assets fs.FS) *Server
```

Pass `nil` for `assets` to run API-only. The server implements `http.Handler`,
so it drops straight into `httptest.Server`.

## A word on access control

There is none. No authentication, and `CheckOrigin` accepts every origin. The
dashboard exposes hostnames, the full process list, disk serial numbers and
network configuration.

Bind it to an interface you trust, or put an authenticating proxy in front of
it. Do not expose it to a network you do not control.
