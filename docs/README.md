# go-sysmon documentation

How the pieces fit together, what each metric actually measures, and where to
look when something reads wrong.

If you just want to run it, the [README](../README.md) is enough. Come here
when you need to know why a number says what it says.

## Components

| Component | What it covers |
|-----------|----------------|
| [Collector](collector/architecture.md) | Reading metrics from procfs, sysfs, hwmon and ioctls |
| [Virtualization](collector/virtualization.md) | Containers and VMs, without a runtime socket |
| [Disk and network detail](collector/disk-and-network.md) | Queue depth, peaks, interface classification, DNS |
| [Sensors](collector/sensors.md) | Where temperatures, power and PSI come from |
| [Monitor](monitor/architecture.md) | Polling, and broadcasting to subscribers |
| [Server](server/architecture.md) | HTTP, server-sent events and TLS |
| [Container](server/container.md) | Running in Docker, and what it needs from the host |
| [API](server/api.md) | Connecting your own client |
| [CLI](cli/reference.md) | Every command and flag |
| [GUI](gui/architecture.md) | The Wails desktop app and table conventions |
| [Types](types/reference.md) | The shared structures everything speaks in |

## Desktop extensions

| Extension | What it does |
|-----------|--------------|
| [Cinnamon applet](extensions/cinnamon.md) | Per-core bars and gauges on the panel, full detail in the popup |
| [System tray](extensions/systray.md) | Live CPU, RAM, swap and GPU in the notification area |
| [Desktop testing](testing/desktop.md) | How the extensions are tested, in three tiers |

## Building, testing, releasing

| Task | Command |
|---|---|
| Run what CI runs | `make ci` (or `make ci-fast` to skip containers and browsers) |
| Scan Go dependencies and stdlib | `make vulncheck` |
| Scan the frontend | `make audit` |
| Build release binaries | `make release-build`, `make release-build-desktop` |
| Deploy to a host | [server/deployment.md](server/deployment.md) |

The workflows themselves are `.github/workflows/ci.yml` and
`.github/workflows/release.yml`. `make ci` runs the same targets in the same
order, so a green local run means a green CI run.
