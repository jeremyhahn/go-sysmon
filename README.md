# go-sysmon

[![CI](https://github.com/jeremyhahn/go-sysmon/actions/workflows/ci.yml/badge.svg)](https://github.com/jeremyhahn/go-sysmon/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/jeremyhahn/go-sysmon.svg)](https://pkg.go.dev/github.com/jeremyhahn/go-sysmon)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Real-time Linux system monitor with a desktop GUI, web dashboard, CLI, system
tray, and Cinnamon panel applet — from a single Go codebase.

Everything is read from the kernel: `/proc`, `/sys`, cgroups and ioctls. There
is no agent, no database, no configuration file, and the web dashboard is
embedded in the binary, so deploying it is copying one file.

---

## Features

### Metrics

| Area | What it reports |
|---|---|
| **CPU** | Per-core usage and frequency, per-core temperature, model, vendor, topology (sockets/cores/threads), cache, microcode, and the full CPU flag list |
| **Memory** | Used/free/available/cached/buffered/slab, swap, and per-DIMM manufacturer, part number, serial, type, speed, voltage and temperature from SMBIOS |
| **Storage** | Per-disk model, serial, size, partitions and usage; SMART and NVMe health (wear level, spare, media errors, power-on hours); temperature; **read/write rates, queue depth, average queue length, %util, and peaks since start** |
| **Network** | Per-interface addresses, MTU, speed, duplex, driver, traffic counters and rates; **interface classification** (ethernet/wifi/bridge/bond/vlan/tun/veth/loopback); operational state distinct from the admin flag; **DNS servers and search domains**; bridge ports; bond mode, slaves and master; wireless signal, quality and noise |
| **GPU** | NVIDIA via NVML (utilisation, memory, temperature, power, clocks, PCIe throughput, ECC), AMD and Intel via sysfs |
| **Processes** | Per-process CPU, memory, I/O rates, priority and state, with counts by state including idle kernel threads |
| **Sensors** | Core temperatures, voltages, RAPL package power, thermal zones, throttle counters, fan speeds and PSI pressure |
| **Containers** | Per-container CPU, memory, PIDs and I/O from cgroup v2 — plus **CPU throttling, OOM kills, memory peak, swap, major faults, PID limits and PSI stall pressure**. No runtime socket required |
| **Virtual machines** | Guest name, UUID, vCPUs, configured and resident memory, accelerator, disk images, host NICs, per-guest disk I/O and network throughput — all from the host side, no guest agent |
| **Images** | Container image inventory, layer sizes, reclaimable space, volumes and build cache (opt-in, see below) |

### Interfaces

- **Web dashboard** — server-sent event streaming with a selectable refresh rate (500ms to 60s), embedded in the binary
- **Desktop GUI** — Wails v2 native window
- **CLI** — formatted terminal output, `--json` on every command, `--refresh` for continuous updates
- **System tray** — live CPU/RAM/swap/GPU/load in the notification area
- **Cinnamon applet** — Cairo-rendered per-core bars and gauges in the panel
- **REST + streaming API** — `GET /api/snapshot` and `GET /api/events` for your own clients, with optional TLS

### Engineering

- **Race-free** — the full suite runs under `-race` in CI
- **Lock-free hot path** — atomics rather than mutexes where it counts
- **Frequency tiering** — expensive collectors (SMART, GPU, images) run less often than cheap ones
- **Quiet by design** — conditions that cannot change during a run are logged once, not once per poll
- **Self-diagnosing** — `sysmon doctor` reports what the build can do on the current host and how to fix what it cannot

---

## Install

Download a binary from [Releases](https://github.com/jeremyhahn/go-sysmon/releases):

```bash
curl -LO https://github.com/jeremyhahn/go-sysmon/releases/latest/download/sysmon-server-0.1.2-linux-amd64
sudo install -m 0755 sysmon-server-0.1.2-linux-amd64 /usr/local/bin/sysmon-server
sysmon-server serve --addr :8080
```

Then open `http://localhost:8080/`.

Or as a container, **on a Linux host**:

```bash
docker run -d --name sysmon \
  --pid=host --network=host \
  -v /sys:/sys:ro -v /sys/fs/cgroup:/sys/fs/cgroup:ro \
  -v /etc/os-release:/etc/os-release:ro \
  ghcr.io/jeremyhahn/go-sysmon:0.1.2 serve --addr :8080
```

The host flags are not optional. Without them the container reports *itself* --
one process, its own veth pair -- rather than the host. Add `--privileged` for
SMART disk health; capabilities alone are not enough. There is a
[docker-compose.yml](docker-compose.yml) with every setting explained, and
[docs/server/container.md](docs/server/container.md) covers the trade-offs.

Two things that catch people out:

- **The host must be Linux.** On Docker Desktop for macOS or Windows the
  container measures the Linux VM it runs in, not your laptop. It starts, and
  the numbers are real -- they are just the VM's. Run the native binary on a
  Linux machine and point a browser at it instead.
- **`--network=host` means `--addr` picks the real host port**, so there is no
  `-p` mapping and nothing warns you if the port is taken -- the container just
  exits. Detached, that looks like nothing happened. `docker logs sysmon` has
  the reason; `--addr :9090` moves it.

---

## Two binaries, one codebase

`make build` produces two binaries from the same source. They share every
subcommand; only their runtime dependencies differ.

| | `bin/sysmon` | `bin/sysmon-server` |
|---|---|---|
| Build tags | `desktop,production,webkit2_41,systray` | none |
| Shared libraries | **129** (GTK, WebKit, libsoup, …) | **3** (libc, ld.so, vdso) |
| Size | ~22 MB | ~15 MB |
| Native GUI window | yes | no |
| CLI and `serve` | yes | yes |
| Runs on a headless host | **no** | yes |

The desktop build is a functional superset — it does everything the server
build does, plus the window. It simply cannot start without a desktop stack:
the dynamic linker resolves those libraries before `main()` runs, so a missing
one is a loader error the program cannot catch and report.

Deploy `sysmon-server` to servers; use `sysmon` on a workstation.

---

## Building from source

### Requirements

- **Go** — the version pinned by the `toolchain` directive in `go.mod` (downloaded automatically)
- **Node.js 22+** — builds the embedded web frontend
- **CGO** — mandatory. `pkg/collector` imports NVIDIA `go-nvml`, which does not compile with `CGO_ENABLED=0`
- **Docker** — only for `make integration-test`

### Wails desktop dependencies

The desktop build embeds a [Wails v2](https://wails.io) application, which links
GTK 3 and WebKit2GTK. These are **build-time and run-time** requirements for
`bin/sysmon` only — `bin/sysmon-server` needs none of them.

```bash
make deps          # installs them for your distribution
```

Or by hand:

| Distribution | Packages |
|---|---|
| Debian / Ubuntu / Mint | `pkg-config build-essential libgtk-3-dev libwebkit2gtk-4.1-dev libsoup-3.0-dev` |
| Fedora / RHEL | `pkgconf-pkg-config gcc gtk3-devel webkit2gtk4.1-devel libsoup3-devel` |
| Arch | `pkgconf base-devel gtk3 webkit2gtk-4.1 libsoup3` |

At **run time** the desktop binary needs the non-`-dev` equivalents
(`libgtk-3-0`, `libwebkit2gtk-4.1-0`, `libsoup-3.0-0`, `libglib2.0-0`).
`sysmon doctor` names anything missing along with the command that installs it.

The `webkit2_41` build tag selects WebKit2GTK 4.1. Disable the tray with
`make build WITH_SYSTRAY=0`.

### Build

```bash
make build           # frontend + both binaries
make build-server    # portable binary only (no GTK/WebKit needed)
make build-desktop   # desktop binary only
```

---

## Usage

```bash
# Web dashboard
sysmon-server serve --addr :8080
sysmon-server serve --addr 10.0.0.5:8080 --interval 500

# Desktop GUI
sysmon

# CLI — every command supports --json and --refresh
sysmon                  # one-shot overview
sysmon cpu              # topology, per-core usage, temperatures, flags
sysmon memory           # usage, swap, per-DIMM detail
sysmon storage          # disks, SMART, queue depth, peaks
sysmon network          # interfaces, DNS, bridges, bonds, wifi
sysmon gpu              # NVIDIA, AMD and Intel
sysmon containers       # containers with throttling and OOM history
sysmon vms              # virtual machines
sysmon images           # image inventory and reclaimable space
sysmon doctor           # what works here, and how to fix what does not
```

Full reference: **[docs/cli/reference.md](docs/cli/reference.md)**.

### Logging

Diagnostics go to stderr by default so `--json` output on stdout stays
parseable. A plain `> file` redirect therefore captures nothing — use the flag:

```bash
sysmon-server serve --log-file /var/log/sysmon/server.log --log-format json
```

`--log-file`, `--log-level` and `--log-format` are global to every subcommand.

### Privileges

sysmon runs unprivileged and degrades gracefully. `sysmon doctor` reports what
is unavailable and why:

| Feature | Requirement |
|---|---|
| SMART / NVMe health, disk temperature | root, or membership of the `disk` group |
| DIMM detail from SMBIOS | root |
| Container image inventory | the `docker` group |
| Container metrics | cgroup v2 (no privileges) |
| VM metrics | none |

---

## Deploying

```bash
make deploy HOST=user@host
```

Checks the target's architecture and glibc, installs to `/usr/local/bin`, and
verifies the binary runs. See **[docs/server/deployment.md](docs/server/deployment.md)**
for the systemd unit and hardening notes.

> **Security**: the dashboard has **no authentication**. It exposes hostnames,
> the full process list, disk serial numbers and network configuration. The
> event stream is subject to the browser's same-origin policy, which stops a
> random web page reading it — that is not a substitute for access control.
> Bind it to a private interface or put an authenticating reverse proxy in
> front of it before exposing it anywhere untrusted.

---

## Documentation

| Document | Contents |
|---|---|
| [docs/](docs/) | Documentation index |
| [docs/cli/reference.md](docs/cli/reference.md) | Every command and flag |
| [docs/cli/architecture.md](docs/cli/architecture.md) | How the CLI renders |
| [docs/collector/architecture.md](docs/collector/architecture.md) | Data flow from sysfs to screen |
| [docs/collector/sensors.md](docs/collector/sensors.md) | Temperature, power and PSI sources |
| [docs/collector/disk-and-network.md](docs/collector/disk-and-network.md) | Queue depth, peaks, interface classification, DNS |
| [docs/collector/virtualization.md](docs/collector/virtualization.md) | Containers, VMs and image inventory |
| [docs/monitor/architecture.md](docs/monitor/architecture.md) | Snapshot aggregation and broadcasting |
| [docs/server/api.md](docs/server/api.md) | REST and event-stream API |
| [docs/server/deployment.md](docs/server/deployment.md) | Build, deploy, systemd, modes |
| [docs/server/container.md](docs/server/container.md) | Docker and Kubernetes, and the privileges each metric needs |
| [docs/gui/architecture.md](docs/gui/architecture.md) | Frontend structure and table conventions |
| [docs/types/](docs/types/) | Shared data structures and error types |
| [docs/extensions/](docs/extensions/) | Cinnamon applet and system tray |

---

## Development

```bash
make ci              # the full pipeline CI runs — green locally means green in CI
make ci-fast         # same, minus the container and browser suites

make test            # unit tests with the race detector
make integration-test# end-to-end CLI tests in a container
make test-e2e        # Playwright browser tests
make lint            # gofmt, go vet and golangci-lint
make vulncheck       # govulncheck: dependencies and the standard library
make audit           # npm audit for the frontend
make coverage-collector   # per-package coverage (also -cli, -monitor, -server, -types)
```

`make ci` runs exactly what the GitHub workflow runs, in the same order, so the
two cannot drift.

### Project layout

```
cmd/sysmon/          CLI, GUI entry point, embedded frontend
  frontend/          Svelte 5 + Tailwind web dashboard
pkg/collector/       Metric collection from /proc, /sys, cgroups and ioctls
pkg/monitor/         Snapshot aggregation and subscriber broadcast
pkg/server/          HTTP server, event streaming and TLS
pkg/cli/             Terminal rendering
pkg/types/           Shared data structures and typed errors
test/integration/    Containerised end-to-end CLI tests
extensions/          Cinnamon applet
```

---

## License

[MIT](LICENSE)
