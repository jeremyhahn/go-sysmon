# go-sysmon specification

> **Status: NORMATIVE** · Scope: go-sysmon · Last verified: 2026-10-21
> Conflicts with this document are defects. Fix them here first.

## Purpose

go-sysmon is a standalone Linux system monitor: CPU, memory, disk and network, live over server-sent events.
One collector feeds a CLI, a web dashboard and a desktop app, and the server embeds as a library in a TLS-enabled application.
It runs as infrastructure on the fleet hosts and aggregates a fleet of them.
It has no in-house module dependencies. Deployment into go-trusted-platform is planned but not designed.

## Boundaries

- Owns Linux system monitoring of the host it runs on: CPU, memory, disks, network, accelerators, sensors, containers and VMs.
- Owns the fleet view built by polling its own peers, and the Prometheus export of its readings.
- Reads containers and VMs from the host side only; never talks to a container runtime or hypervisor socket.
- Never runs on, or monitors, anything but Linux.
- Never adds a stack dependency, and never builds toward the go-trusted-platform integration before that design is recorded.
- Streams one way over server-sent events; never uses WebSockets without a real duplex requirement.

## Features

| Feature | Status | Requirements | Detail |
|---|---|---|---|
| cpu-metrics | shipped | Show every CPU with per-core usage, temperature and frequency. Show the manufacturer, model, speed, caches and flags. | docs/collector/ |
| memory-metrics | shipped | Show used, cached, buffered, free and swap memory with swap paging. Show the module manufacturer and speed where the host exposes them. | docs/collector/ |
| disk-metrics | shipped | Show every disk with its model, specs, usage, rates, queue depth and SMART health. Show peak throughput since start. Tell a missing device from an unreadable one. | docs/collector/ |
| drive-slot | shipped | Read per-bay drive health from the BMC (Redfish, IPMI fallback), read-only; alert on fault or predicted failure. | docs/collector/drive-slots.md |
| network-metrics | shipped | Show every interface with its addresses, netmask, DNS, state and rates in bits per second. Classify bridges, bonds, VLANs and wifi adapters, and show the wifi link detail. Show the NIC manufacturer and model. | docs/collector/ |
| accelerator-metrics | shipped | Show GPUs and NPUs with usage, memory and temperature. NVIDIA detail needs the `nvml` build tag. | docs/collector/ |
| virtualization | shipped | List containers, VMs and images from the host side, without a runtime socket. Hide those tabs when the host has neither. | docs/collector/ |
| monitor | shipped | Poll the collector on one loop and broadcast each snapshot to every subscriber. Change the interval at run time. | docs/monitor/ |
| event-stream | shipped | Stream snapshots over `GET /api/events`, with a client-chosen interval of 500ms to 60s through `POST /api/interval`. Send deltas, and never render a stale number. | docs/server/ |
| tls | shipped | Serve TLS from a certificate and key path, or from a per-handshake `GetConfigForClient` hook when embedded. | docs/server/ |
| http-api | shipped | Serve snapshots, summaries and history over JSON with security headers and a same-origin policy. Expose liveness and readiness probes. Believe forwarding headers only from trusted proxies. | docs/server/ |
| configuration | shipped | Read settings from a file and reload them without a restart. | docs/server/ |
| alerts | shipped | Evaluate threshold rules from the configuration and show what fired. | docs/server/ |
| fleet | shipped | Poll peer summaries and roll the fleet up, composing aggregators into a tree. Page the fleet view server-side and never publish peer addresses on unauthenticated endpoints. | docs/server/ |
| prometheus-metrics | shipped | Export the readings and alert evaluation on `/metrics`, counters as counters. Chart history back from Prometheus when configured. Export NVMe write and read volume. | docs/server/ |
| cli | shipped | Show every subsystem from the terminal, one-shot or refreshing, as text or JSON. Run the server, the doctor check and the desktop app. | docs/cli/ |
| gui | shipped | Show the same pages in the Wails desktop app and the browser dashboard, with charts, tables, tabs and the fleet view. | docs/gui/ |
| desktop-extensions | shipped | Show live readings in a Cinnamon panel applet and in the system tray. | docs/extensions/ |
| wire-types | shipped | Define one set of snapshot types for every client, with the dashboard's types generated from the Go structs. Never send a list as null. | docs/types/ |
| container-image | shipped | Run in Docker or Kubernetes against the host's procfs and sysfs, with a healthcheck. | docs/server/ |
| remote-deployment | shipped | Install on a remote host with `make deploy` as a systemd service under its own account, with a watchdog. The watchdog notification never blocks. | docs/deployment/ |
| ci-pipeline | in progress | Vet, lint, test and scan every shipped build-tag set, with tools at pinned versions. Split CI into a PR tier and a full tier. Run CI without the host Docker socket. | docs/testing/ |
| release-pipeline | shipped | Assemble the changelog from fragments and release from a signed tag on `main` that matches `VERSION`. | none |
| release-signing | planned | Sign the release artifacts. | none |
| project-docs | in progress | Carry this SPEC.md, an agreeing ROADMAP.md and a feature folder per feature. | none |

## Interfaces

- CLI: `sysmon` and its subcommands; see [cli/reference.md](cli/reference.md).
- HTTP API and event stream: see [server/api.md](server/api.md).
- Go library: the server and collector packages, embedded with static or dynamic TLS; see [server/architecture.md](server/architecture.md).
- Wire types: see [types/reference.md](types/reference.md).
- Prometheus: `/metrics`; see [server/alerting-fleet-config.md](server/alerting-fleet-config.md).
- Desktop: the Wails app and the extensions; see [gui/user.md](gui/user.md) and [extensions/](extensions/).

## Release criteria

- Every issue on the Release candidate milestone ([ROADMAP.md](../ROADMAP.md)) is closed by a merged PR, except the tag mechanics.
- CI on `main` is green.
- The owner has approved this SPEC.md and ROADMAP.md, and they agree.
- The release candidate is cut by the documented release steps: `VERSION` set, changelog assembled, signed tag on `main`.
- Release signing is not a criterion; it is deferred.
