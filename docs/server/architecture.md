# Server architecture

`pkg/server` puts the metrics on HTTP. It collects nothing itself — every
response comes from the monitor.

## Endpoints

| Method | Path            | What it does                                        |
|--------|-----------------|-----------------------------------------------------|
| GET    | `/`             | The dashboard, or a plain status line when no assets are embedded |
| GET    | `/api/snapshot` | The latest snapshot as JSON                         |
| GET    | `/api/events`   | A server-sent events stream of snapshots            |
| GET    | `/api/interval` | The monitor's current polling interval              |
| POST   | `/api/interval` | Changes the polling interval                        |

## Why server-sent events rather than WebSocket

The data flow is one-way. The server pushes snapshots; the only thing a client
ever needed to send was a rate change, and that is a request/response operation
that belongs on a REST endpoint. Nothing here needs a duplex channel.

Choosing SSE buys three things:

- **The stream stays inside the normal handler chain.** It is an ordinary HTTP
  response, so the same-origin policy, CORS, ordinary middleware and response
  compression all apply without special cases. A WebSocket handshake bypasses
  the same-origin policy entirely, which meant any page a user visited could
  read this server.
- **Compression that actually pays.** Consecutive snapshots are nearly
  identical — same JSON keys, same process names, same interface names, only
  the numbers move. Because the deflate dictionary carries across flushes, each
  snapshot after the first compresses against the one before it.
- **Less machinery.** No ping/pong keepalive, no close frames, no third-party
  dependency. The keepalive is a comment line and the shutdown signal is an
  event.

The costs are real but do not bite here: SSE is UTF-8 text only, so a binary
payload would need base64, and over HTTP/1.1 the stream occupies one of the
browser's six connections per origin. The payload is JSON and there is one
stream per tab.

## The event stream

`GET /api/events` returns `text/event-stream` and starts sending immediately.
Frame types, compression and reconnect semantics are documented in the
[API reference](api.md#get-apievents).

Two details worth knowing here:

**The first snapshot is the cached one.** The handler sends
`monitor.Snapshot()` before entering the subscription loop, so a reconnecting
client renders straight away instead of waiting a full poll interval. At a 60s
interval that is the difference between an instant dashboard and a blank one.

**Write deadlines are per-event.** `http.ResponseController.SetWriteDeadline`
bounds each write to 10 seconds. A writer that does not support deadlines is
not an error — the deadline is hardening against a stalled peer, not a
correctness requirement.

### Shutdown

When the monitor stops it closes every subscriber's `Done` channel. Each stream
handler sends `event: bye` and returns.

This ordering matters: an event stream never ends on its own, so
`http.Server.Shutdown` would block on those handlers until its timeout expired.
Stopping the monitor first releases them, and `serveUntilSignal` does exactly
that before calling `Stop`.

## TLS

The server speaks plain HTTP unless configured otherwise. `NewWithConfig`
accepts a `TLS` value supporting three cases, which compose:

```go
srv, err := server.NewWithConfig(server.Config{
    Monitor: mon,
    Addr:    ":8443",
    Assets:  assets,
    TLS: &server.TLS{
        CertFile: "/etc/ssl/sysmon.crt",
        KeyFile:  "/etc/ssl/sysmon.key",
    },
})
```

| Field                | Use                                                        |
|----------------------|------------------------------------------------------------|
| `CertFile`, `KeyFile`| The classic static case: a PEM pair on disk. Both or neither |
| `Config`             | A caller-supplied `*tls.Config`, cloned before use          |
| `GetConfigForClient` | Resolves the configuration per handshake                    |

**TLS is negotiated once per connection, before any HTTP request exists**, so
the dynamic hook fires per handshake rather than per request. It receives the
`tls.ClientHelloInfo`, which carries the SNI server name, the offered ALPN
protocols, the cipher suites and the client address — enough to pick a
certificate per tenant. Returning a non-nil config replaces the base
configuration for that connection entirely, so it must carry its own
certificates; include `h2` and `http/1.1` in its `NextProtos` to keep HTTP/2
available.

A caller-supplied `Config` is cloned, never mutated. `MinVersion` defaults to
TLS 1.2 when the caller leaves it at zero. A configuration with no way to
produce a certificate is rejected at construction with a `types.TLSConfigError`
rather than failing later as an opaque handshake error.

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
// Plain HTTP, the common case.
func New(mon *monitor.Monitor, addr string, assets fs.FS) *Server

// Everything else, including TLS.
func NewWithConfig(cfg Config) (*Server, error)
```

Pass `nil` for `assets` to run API-only. The server implements `http.Handler`,
so it drops straight into `httptest.Server` — and an application embedding
go-sysmon can mount it on its own router and terminate TLS itself, ignoring
`Start` entirely.

## A word on access control

There is none. No authentication.

The event stream is subject to the same-origin policy, so a random web page
cannot read it cross-origin. That is not a substitute for authentication:
anything that can route to the port and is not a browser reads everything —
hostnames, the full process list, disk serial numbers, network configuration.

Bind it to an interface you trust, or put an authenticating proxy in front of
it. Do not expose it to a network you do not control.
