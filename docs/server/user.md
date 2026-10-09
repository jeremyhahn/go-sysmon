# Server — using it

> **Status: NORMATIVE** · Scope: running and operating `sysmon serve` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

## Prerequisites

- The server binary, `sysmon-server` (or `sysmon`); build and install steps are
  in [deployment.md](deployment.md). Install as a supervised service with
  [deployment/user.md](../deployment/user.md), or run it in a container
  ([container.md](container.md)).
- A bind address on an interface you trust. There is **no authentication**:
  anything that can reach the port can read hostnames, the process list, disk
  serial numbers and network configuration. Bind to loopback, or put an
  authenticating proxy in front.

## First run

```bash
sysmon-server serve --addr 127.0.0.1:8080
```

Open `http://127.0.0.1:8080`. For a quick check from a shell:

```bash
curl -s http://127.0.0.1:8080/api/version
curl -s http://127.0.0.1:8080/api/snapshot | jq .host
```

## Common tasks

- **Change the refresh rate.** The dropdown in the dashboard, or
  `curl -X POST -d '{"interval_ms":5000}' http://host:8080/api/interval`. It is
  shared by everyone viewing the node.
- **Serve HTTPS.** Add `--tls-cert` and `--tls-key`, and use `https://`.
- **Use a config file.** Put settings in `/etc/go-sysmon/sysmon.yaml` (or pass
  `--config`). Reload it with `kill -HUP <pid>` or `systemctl reload`; set
  `server.watch: true` to reload when the file changes. The dashboard's
  settings view shows what is in force; the journal lists settings that await a
  restart.
- **Set alert thresholds.** The `alerts:` section of the config file
  ([alerting-fleet-config.md](alerting-fleet-config.md)).
- **Watch many machines.** List them under `fleet.peers`; the dashboard shows a
  fleet view ([alerting-fleet-config.md](alerting-fleet-config.md)).
- **Scrape or keep history.** `/metrics` and the history store are described in
  [alerting-fleet-config.md](alerting-fleet-config.md); they exist only in
  builds with Prometheus support.
- **Write your own client.** [api.md](api.md).
- **Stop it.** `Ctrl+C` or `SIGTERM`; connected browsers are told the stream is
  closing.

## Troubleshooting

- **`Error: … address already in use`.** Another process holds the port; pick
  another `--addr`.
- **`Error: … TLS`.** The certificate and key must be given together, be
  readable and match.
- **The dashboard loads but data is old.** Check the interval; a 60 s interval
  updates once a minute.
- **Fleet peers show unreachable.** Check the peer URL, firewall and, for HTTPS
  peers, the `fleet.tls` settings; a bad CA file stops startup rather than
  appearing as unreachable peers.
- **A setting did not change after reload.** The journal lists settings that
  need a restart (for example the listen address).
- **Browser behind a proxy sees no live updates.** The proxy is buffering the
  stream; disable response buffering for `/api/events`.
- **Blank SMART or temperature values.** The process lacks permission; run
  `sysmon doctor` as the same user ([cli/user.md](../cli/user.md)).
