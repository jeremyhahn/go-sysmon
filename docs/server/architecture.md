# Server architecture

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the server package.

`pkg/server` puts the metrics on HTTP. It collects nothing itself — every
response comes from the monitor.

## Endpoints

| Method | Path            | What it does                                        |
|--------|-----------------|-----------------------------------------------------|
| GET    | `/`             | The dashboard, or a plain status line when no assets are embedded |
| GET    | `/api/snapshot` | The latest snapshot as JSON                         |
| GET    | `/api/summary`  | This node's fleet summary, from its two latest snapshots |
| GET    | `/api/snapshot/history` | The latest snapshot without process list or CPU flags, for fleet history |
| GET    | `/api/events`   | A server-sent events stream of snapshots            |
| GET    | `/api/fleet/subtree` | This aggregator's node index and composable roll-up, for its parent |
| GET    | `/api/fleet/nodes` | Named nodes of this fleet, for a parent filling a page |
| GET    | `/api/interval` | The monitor's current polling interval              |
| POST   | `/api/interval` | Changes the polling interval                        |

## Every response is the poll the monitor last published

Owner ruling, 2026-09-04: *"the poller does its job, updates a state, and every
API request and stream serves from that state until the next poll. No endpoint
may query fresh host state per request."*

That is how the package is built. `Monitor.Snapshot()` is `m.latest.Load()` — an
atomic pointer read of the last completed collection — and every handler goes
through it: `/api/snapshot`, the event stream, the delta hub's first frame, and
the Prometheus exporter at `/metrics`. `/api/summary` and `/api/snapshot/history`
read the same state through `Monitor.Consecutive()`, which pairs it with the
collection before it. `/api/fleet` serves a page of the fleet poller's cached
peer summaries, and `/api/fleet/rollup` the whole-fleet roll-up over them, and `/api/fleet/{name}/snapshot` fetches a peer's published snapshot
from that peer when someone drills in (#76), which is the peer's own last poll
and not fresh host state; `/api/history` queries storage, which is not host state.
On a tree of aggregators (#190), a parent serves ordering, search, counts and
routing from the child indexes it polled. The second sanctioned exception, of the
same kind as drill-in (owner ruling 2026-09-23), is that only a page's subtree
summaries are fetched from their children on request, and reused for the poll
interval. History for a subtree node is relayed to the child that records it.
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

### Every path serves the same decorated snapshot

The fields that belong to the service rather than the hardware — `version`,
`git_commit`, `build_date`, `node_name` and `alerts` — are attached in one
place: the server's `snapshotDecorator` (`pkg/server/decorate.go`), which the
server installs on its monitor with `Monitor.SetDecorator` when it is
constructed. The monitor applies it to each collection before publishing, so
`/api/snapshot`, both event stream protocols, the fleet's view of this node,
`/metrics` and the exporter all read one decorated value. This holds for a
server built by `sysmon serve` and for one embedded with `server.New` or
`NewWithConfig` alike; the build fields come from `server.Config` (empty for
`server.New`), and alert thresholds from `Config.Config`, or the built-in
defaults without one.

A monitor carries **one** decorator, because its snapshots describe one node.
A second server built over the same monitor with the same build, configuration
and fleet shares the first's decorator — and its alert evaluator, so a
condition's duration is not restarted. One that describes the node differently
is refused by `NewWithConfig` with `DecoratorInstalledError` rather than
silently overwriting the first's build, name and alert history. `server.New`,
which describes nothing of its own, serves whatever decoration the monitor
already has, or installs the default one. `sysmon serve` starts its monitor
only after the server is built, so no collection is ever published
undecorated; a server built over an already-running monitor decorates the
published snapshot before its constructor returns.

The monitor also dates a reading whose Snapshotter left `timestamp` unset,
since a reading is identified by its timestamp (below).

These were attached by the CLI's own snapshotter until #162, so an embedded
server streamed none of them and its `/api/events` and `/api/snapshot`
disagreed. `pkg/server/served_snapshots_test.go` builds both shapes and fails
if any path's snapshot differs from another's in any field.

A reload calls `Monitor.Restamp`, which re-applies the decorator's `Identify`
to the published snapshot **before returning**, so a rename of the node
(`fleet.local_name`) is reported on every path at once rather than at the next
collection — which may be a minute away, or never while a collector is hung.
Publishing is serialised by a lock the collection itself is outside of, so a
restamp can neither overwrite a newer collection nor be overwritten by one
identified under the old configuration, and a hung collector cannot delay it.
A reload that changes nothing `Identify` reads publishes nothing.

A restamp is broadcast, so streams carry the new name too, but it is the
**same reading**: same `timestamp`, same counters, alerts as evaluated (the
evaluator keeps state between readings and sees each exactly once). A stream
client keeping history must therefore key it by `timestamp` and add one point
per reading; the dashboard does (`src/lib/reading.ts`). Installing a decorator
on a monitor that has already published decorates that snapshot the same way,
evaluating too, so a server built after its monitor started serves nothing
undecorated.

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

## Security headers

`securityHeaders` in `pkg/server/headers.go` wraps the whole mux, so every
response gets the browser security headers listed in
[the API reference](api.md#security-headers). It sits outside the mux on
purpose: a route added later, the mux's own 404 and 405 answers, the static
files and the event stream all pass through it without having to remember
anything. It sets headers before the handler runs, so a handler still sets its
own `Content-Type` and `Cache-Control`, and it does not wrap the
`ResponseWriter`, so the event stream keeps its flushing and per-event write
deadlines.

`Strict-Transport-Security` follows the request, not the configuration: it is
sent when `r.TLS` is set. That covers both of the server's own TLS modes and an
embedding application that terminates TLS and mounts the server as an
`http.Handler`, and it is correct behind a TLS-terminating proxy, where the
request really is plain HTTP and the proxy owns HSTS.

### The policy and the dashboard

The Content-Security-Policy allows no inline script or style, and the dashboard
works under it with nothing hashed or exempted:

- The pre-paint theme bootstrap is `public/theme-init.js`, loaded as a classic
  script from `<head>`. Vite copies files in `public/` verbatim; a bundled
  module would run after first paint, which is the flash it exists to prevent.
- Chart tooltips are built as DOM nodes (`src/lib/tooltip.ts`) and returned to
  ECharts as an element, not as an HTML string. `style-src` governs style
  attributes parsed from markup, so a `<span style="color:…">` swatch would be
  refused; a colour set through the CSSOM (`element.style.color`) is not
  governed by it. Series names are set as text, never parsed as markup.
- Svelte applies dynamic `style` bindings through `element.style.cssText`, and
  ECharts sizes its canvas and tooltip the same way — both CSSOM writes.
- `img-src` allows `data:` because daisyUI draws its select arrows and checkbox
  marks as `data:` SVG images from the stylesheet. An image cannot run script.

`tests/csp.spec.ts` opens every tab, hovers every chart, opens the date range,
settings and alert surfaces, and fails on any `securitypolicyviolation` event
or any Chromium console warning about the policy headers.

### Changing them

An embedder changes the set through `Config.SecurityHeaders`. Each header named
there replaces the default of the same name (names are matched canonically) or
is added; a header named with no values is removed. `DefaultSecurityHeaders`
returns a fresh copy to start from.

```go
headers := server.DefaultSecurityHeaders()
// Frame the dashboard inside the platform's own UI.
headers.Set("Content-Security-Policy", strings.Replace(
    headers.Get("Content-Security-Policy"),
    "frame-ancestors 'none'", "frame-ancestors https://platform.example", 1))

srv, err := server.NewWithConfig(server.Config{
    Monitor:         mon,
    Assets:          assets,
    SecurityHeaders: headers,
})
```

A name that is not a valid field name, a value with a control character, or one
header named twice under two spellings fails construction with a
`types.SecurityHeaderError` rather than being rewritten on the wire.

## Constructing one

```go
// Plain HTTP, the common case.
func New(mon *monitor.Monitor, addr string, assets fs.FS) *Server

// Everything else, including TLS.
func NewWithConfig(cfg Config) (*Server, error)
```

Pass `nil` for `assets` to run API-only. Both constructors install the
[security headers](#security-headers). The server implements `http.Handler`,
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

The [security headers](#security-headers) protect a browser viewing the
dashboard -- against framing, content sniffing and injected script. They do not
restrict who can read the API.
