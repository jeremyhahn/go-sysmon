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

## Every response is the poll the monitor last published

Owner ruling, 2026-09-04: *"the poller does its job, updates a state, and every
API request and stream serves from that state until the next poll. No endpoint
may query fresh host state per request."*

That is how the package is built. `Monitor.Snapshot()` is `m.latest.Load()` — an
atomic pointer read of the last completed collection — and every handler goes
through it: `/api/snapshot`, the event stream, the delta hub's first frame, and
the Prometheus exporter at `/metrics`. `/api/fleet` serves the fleet poller's
cached peer snapshots; `/api/history` queries storage, which is not host state.
**`pkg/server` does not import a collector at all.**

The cost of getting this wrong is not abstract: two clients would measure the
machine twice for one poll, and a slow read — a disk that has spun down, a
`smartctl` that takes eight seconds — would block a response that had a
perfectly good reading available.

### The rule is enforced, not described

`pkg/server/architecture_test.go` walks the first-party import graph and fails
if `pkg/server` can reach `pkg/collector` through **any** chain. Transitively,
not just directly: the question is what the request path can do, and a package
two hops away reads the host just as easily as one imported here. When it fails
it prints the shortest chain, so the answer arrives with the finding.

A second test asserts the graph actually loaded. Every check of the form "X
cannot reach Y" passes when nothing was read, and a guard that reports a pass
having read nothing is the failure this repository keeps finding elsewhere.

This is a test rather than a paragraph for a measured reason. The equivalent
rule for the frontend bundle **was** written down — "a change to the frontend is
not complete until `dist/` is rebuilt and committed with it" — and `main` still
spent a day carrying a bundle that did not match its sources (#55, #89).
**Prose does not enforce; a failing check does.**

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
bounds each write to 10 seconds, and resetting it before every event is what
exempts the stream from the server-wide response deadline below. A writer that
does not support deadlines is not an error — the deadline is hardening against
a stalled peer, not a correctness requirement.

### Shutdown

When the monitor stops it closes every subscriber's `Done` channel. Each stream
handler sends `event: bye` and returns.

This ordering matters: an event stream never ends on its own, so
`http.Server.Shutdown` would block on those handlers until its timeout expired.
Stopping the monitor first releases them, and `serveUntilSignal` does exactly
that before calling `Stop`.

## Connection deadlines

Three deadlines, each bounding a different phase, and the gaps between them are
deliberate.

| Setting | Value | Bounds |
|---|---|---|
| `ReadHeaderTimeout` | 10s | how long a client may take to send request headers |
| `IdleTimeout` | 120s | how long an idle keep-alive connection is held open |
| `WriteTimeout` | 30s | a whole ordinary response: the handler's work plus the body reaching the socket |

There is deliberately **no `ReadTimeout`**: it would cap the request body *and*
the response, and nothing inside a handler can lift it, so `/api/events` could
not exist.

`WriteTimeout` looks like the same problem and is not, because a handler *can*
replace it. The event stream resets its own write deadline before every frame,
so a stream is bounded per event rather than in total — which is the property a
stream actually wants. Every other handler inherits the 30 seconds. This holds
over HTTP/2 as well as HTTP/1.1: `SetWriteDeadline` is supported on an HTTP/2
response and resetting it replaces the server-wide deadline the same way
(measured, not assumed — a stream would otherwise die every 30 seconds over
HTTPS and only over HTTPS).

The reason it is there at all: without a write deadline, a peer that goes away
without sending an RST leaves the response parked in TCP retransmission for as
long as the kernel will retry, holding a goroutine and a connection with nothing
to release either. On vm-host-2 on 2026-09-03, 53 goroutines were parked in
`net/http.checkConnErrorWriter.Write`, 44 of them for the same seven minutes.

30 seconds is a release valve, not a service-level target. The slowest
legitimate response is a history query, which is separately bounded by
`prometheus.timeout_ms` (10s by default). Raise that above 30s and this deadline
becomes the tighter of the two.

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

The dashboard is served from an embedded filesystem by `assetHeaders` in
`pkg/server/assets.go`. Two cache policies, split on whether the filename is
content-addressed:

- A name matching `<base>-<hash>.<ext>` under `/assets/` is `immutable` with a
  long max-age. Today that is the ECharts vendor chunk alone. A new build of it
  produces a new name, so a cached copy can never be the wrong one.
- Everything else is `no-cache, must-revalidate`: `index.html`, and — since
  #55 — the application chunk `/assets/index.js` and its stylesheet
  `/assets/index.css`, which now carry fixed names so that two branches
  touching the frontend no longer collide on three generated paths at once.
  A stale copy of any of these runs old code against a new server, so they must
  be revalidated on every load.

Revalidation is only cheap if there is something to revalidate against. An
embedded filesystem reports a zero modification time, so `Last-Modified` is
useless and a conditional request would carry no validator at all — every
reload would re-download the whole bundle. `assetETags` therefore hashes every
embedded file once at construction and the middleware sets a strong `ETag`,
which `http.ServeContent` reads back to answer `If-None-Match` with a 304.

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
