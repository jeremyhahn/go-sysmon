# Server — implementation contract

> **Status: NORMATIVE** · Scope: `pkg/server`, `cmd/sysmon/cmd_serve.go`, `serve_*.go`, `pkg/config` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

Design, TLS, headers and deadlines are in [architecture.md](architecture.md);
endpoints in [api.md](api.md). This page is the contract a change must keep.

## Exported surface

| Symbol | Contract |
|---|---|
| `server.New(mon, addr, assets) *Server` | Plain HTTP. `assets == nil` serves a status line at `/` and the API only |
| `server.NewWithConfig(cfg Config) (*Server, error)` | Everything else. Errors: `types.TLSConfigError`, `types.SecurityHeaderError`, `types.DecoratorInstalledError` |
| `server.Config` | `Monitor` (required), `Addr`, `Assets`, `TLS`, `Fleet`, `Config`, `Exporter`, `History`, `SecurityHeaders`, `Version`, `GitCommit`, `BuildDate`, `Alerts`. Every field except `Monitor` is optional |
| `server.TLS` | `CertFile`/`KeyFile` (both or neither), `Config *tls.Config` (cloned), `GetConfigForClient` (per handshake). The three compose |
| `(*Server).Listen() (net.Addr, error)` | Binds and returns the bound address (port 0 resolves here) |
| `(*Server).Start() error` | Serves on the bound listener, over TLS when configured; blocks. A clean shutdown returns nil |
| `(*Server).Stop(ctx) error` | Graceful shutdown within `ctx` |
| `(*Server).UsesTLS() bool`, `Addr() string` | Scheme and bound address (`""` before bind) |
| `(*Server).ServeHTTP` | The server is an `http.Handler`, so an embedder can mount it and terminate TLS itself |
| `server.DefaultSecurityHeaders()` | A fresh copy of the default header set |

## Invariants

- **Every response is the monitor's last published poll.** No handler collects
  host state per request. `pkg/server` must not import `pkg/collector`, directly
  or transitively; `architecture_test.go` enforces it.
- **One decorated snapshot.** Build, node name and alerts are attached by the
  monitor's decorator, so `/api/snapshot`, both stream protocols, the fleet view
  and `/metrics` agree field for field.
- **One decorator per monitor.** A second, different description of the node is
  refused rather than overwritten.
- **Events.** `GET /api/events` sends the cached snapshot first, then one event
  per publication; each write has its own 10 s deadline; `event: bye` is sent
  when the monitor stops, which is why shutdown stops the monitor first.
- **Timeouts.** `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s, `WriteTimeout`
  30 s for ordinary responses; no `ReadTimeout`.
- **Request bodies** to `POST /api/interval` are capped at 1 KiB.
- **Allowed intervals (ms):** 250, 500, 1000, 5000, 10000, 15000, 30000, 60000.
- **Security headers** wrap the whole mux; HSTS follows `r.TLS`.
- **Config is read-only over HTTP.** Reload happens through `SIGHUP` or the
  file watcher, never a request.
- **Startup order in `serve`:** signal handler installed first; config loaded;
  monitor built; reload applier registered; fleet poller started; server built
  (installs the decorator); monitor started; socket bound; serve until signal.
- **Shutdown order:** stop watchdog pings, stop the monitor (releasing streams),
  then `Stop` with a 10 s deadline.

## Error semantics

| Source | Result |
|---|---|
| Malformed or oversize interval body | 400 `malformed request body` |
| Interval not in the allowed set, unknown scope, `fleet` scope on a node without fleet | 400 with the `types.InvalidIntervalError` text |
| Unusable TLS configuration | Construction fails with `types.TLSConfigError`; the process does not start |
| Address in use / unbindable | `Listen` fails before serving; `serve` exits 1 |
| Fleet peer CA file unreadable | Startup error (not reported later as "peers unreachable") |
| `fleet.tls.insecure_skip_verify` | Allowed, with a startup warning that peer verification is off |
| `--interval` ≤ 0, or `server.interval_ms` ≤ 0 on reload | `types.InvalidIntervalError`; a bad reload keeps the previous settings |

Handler-level status codes for every endpoint are in [api.md](api.md).

## Configuration

Precedence, highest first: command-line flag, `SYSMON_*` environment variable
(key path upper-cased with dots as underscores, e.g. `SYSMON_SERVER_ADDR`,
`SYSMON_ALERTS_DISK_FULL_PCT`), the YAML file (`/etc/go-sysmon/sysmon.yaml` or
`--config`; missing is not an error), built-in defaults (`config.Defaults()`).

| Key | Default | Applied |
|---|---|---|
| `server.addr` | `:8080` | restart |
| `server.interval_ms` | `1000` | live on reload |
| `server.tls_cert`, `server.tls_key` | *(none)* | restart |
| `server.watch` | `false` | re-reads the file when it changes on disk; an empty file seen by the watcher is refused and the running configuration kept |
| `fleet.*`, `alerts.*`, `prometheus.*` | see [alerting-fleet-config.md](alerting-fleet-config.md) | per key; settings needing a restart are logged as deferred |

## Persistence

None in the server. History, when enabled, is written to an external
Prometheus-compatible store and queried back; see
[alerting-fleet-config.md](alerting-fleet-config.md).

## Test strategy

Unit tests (`pkg/server/*_test.go`, `cmd/sysmon/serve_*_test.go`) drive handlers
in-process (`httptest` and real loopback listeners), with the TLS pairs
generated in the test. `architecture_test.go` guards the import rule;
`served_snapshots_test.go` fails if two paths serve different snapshots;
`sseclient_test.go` exercises the stream. Run `make test-server` and
`make test-cmd-sysmon`; integration and Playwright:
`make integration-test-server`, `make test-e2e`
([testing/integration.md](../testing/integration.md)).

## Observability

- Logs: `starting sysmon web server` (address, interval, config path, peer
  count, build), `server listening` with the bound address, `config:
  reloaded on SIGHUP`, deferred-setting and rejected-setting warnings, and
  shutdown lines.
- `GET /metrics` (Prometheus builds) exposes the exporter's health.
- systemd: `READY=1`, status text and watchdog pings via `sd_notify` when the
  unit provides a notification socket; absent a socket the server runs
  unwatched and warns.
