# Server — CLI

> **Status: NORMATIVE** · Scope: `sysmon serve` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The server has one verb, `serve`. The other verbs and the global flags are in
[cli/cli.md](../cli/cli.md); the server-only configuration keys are in
[engineering.md](engineering.md#configuration).

## `sysmon serve`

Starts the HTTP server: dashboard, JSON API and the server-sent event stream.
Runs until `SIGINT` or `SIGTERM`. No positional arguments.

| Flag | Type | Default | Required | Description |
|---|---|---|---|---|
| `--addr` | string | `:8080` | no | Listen address (`host:port`; port `0` lets the kernel choose and the bound address is logged) |
| `--interval` | int | `1000` | no | Polling interval in milliseconds; at least `500` (`IntervalBelowFloorError`) |
| `--tls-cert` | string | *(none)* | with `--tls-key` | PEM certificate; enables HTTPS |
| `--tls-key` | string | *(none)* | with `--tls-cert` | PEM key matching `--tls-cert` |
| `--trusted-proxies` | strings | *(none)* | no | CIDR prefixes of reverse proxies whose `Forwarded` / `X-Forwarded-*` headers are believed; repeat or comma-separate. See [deployment.md](deployment.md#trusted-proxies) |
| `--config` | string | `/etc/go-sysmon/sysmon.yaml` | no | YAML configuration; a missing file is not an error |

Global `--log-file`, `--log-level` and `--log-format` apply. `--json`,
`--refresh` and `--no-tray` have no effect on `serve`.

Flags win over `SYSMON_*` environment variables, which win over the file, which
wins over defaults.

### Signals

| Signal | Effect |
|---|---|
| `SIGINT`, `SIGTERM` | Graceful shutdown: streams are told `bye`, connections drain for up to 10 s. Honoured from the first instant of startup |
| `SIGHUP` | Reloads the configuration file. A failed reload keeps the previous settings; settings that need a restart are logged and deferred |

### Exit status

`0` after a clean shutdown, `1` on any startup or serve error (bad flag value,
invalid TLS pair, unreadable peer CA file, address in use), printed as
`Error: <message>` on stderr.

### Examples

```bash
# Plain HTTP on the default port
sysmon serve

# Another port, five-second polling
sysmon serve --addr :9090 --interval 5000

# HTTPS with a static certificate
sysmon serve --addr :8443 --tls-cert /etc/ssl/sysmon.crt --tls-key /etc/ssl/sysmon.key

# Explicit config file, then reload it without restarting
sysmon serve --config /etc/go-sysmon/sysmon.yaml &
kill -HUP "$!"

# Settings from the environment
SYSMON_SERVER_ADDR=127.0.0.1:9090 sysmon serve
```

Dynamic TLS (`GetConfigForClient`) is a Go-library option, not a flag; see
[architecture.md](architecture.md#tls).
