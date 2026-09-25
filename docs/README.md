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
| [Deployment](deployment/user.md) | Installing it on a remote host as a systemd service |
| [Alerts, fleet, Prometheus](server/alerting-fleet-config.md) | Thresholds, aggregation, and the optional history path |
| [Delta streaming](server/delta-streaming.md) | Sending what changed, and never showing a stale number |
| [API](server/api.md) | Connecting your own client |
| [CLI](cli/reference.md) | Every command and flag |
| [GUI](gui/architecture.md) | The Wails desktop app and table conventions |
| [House style](gui/house-style.md) | Colour, type, spacing and theming — the reference a sibling project implements against |
| [Types](types/reference.md) | The shared structures everything speaks in |

## Desktop extensions

| Extension | What it does |
|-----------|--------------|
| [Cinnamon applet](extensions/cinnamon.md) | Per-core bars and gauges on the panel, full detail in the popup |
| [System tray](extensions/systray.md) | Live CPU, RAM, swap and GPU in the notification area |
| [Desktop testing](testing/desktop.md) | How the extensions are tested, in three tiers |

## Testing

| Topic | What it covers |
|-------|----------------|
| [Where testing effort pays](testing/writing-tests.md) | Five rules learned the expensive way, with the bugs that taught them |
| [Coverage](testing/coverage.md) | How unit and integration coverage are measured per package, the 90% floor, and where each package stands |
| [Integration tests](testing/integration.md) | The devcontainer the integration suite runs in, and its make targets |
| [Build shapes](testing/build-shapes.md) | Every build-tag set the project ships, and the gates that vet, lint and test each one |
| [CI failure artifacts](testing/ci-artifacts.md) | What a failed CI job keeps (Playwright traces, screenshots and error context, Go test logs, desktop session logs) and how to download it from Gitea or GitHub |
| [Latest toolchain](testing/toolchain.md) | Keeping Go, the dev tools, Node.js, the base images and the actions on their latest stable release, and the daily check that files an issue when one falls behind |

## Building, testing, releasing

| Task | Command |
|---|---|
| Run what CI runs | `make ci` (or `make ci-fast` to skip containers and browsers) |
| Scan Go dependencies and stdlib | `make vulncheck` |
| Scan the frontend | `make audit` |
| Build release binaries | `make release-build`, `make release-build-desktop` |
| Deploy to a host | [deployment/user.md](deployment/user.md) — `make deploy` |
| Build prerequisites and the bare-binary path | [server/deployment.md](server/deployment.md) |

The workflows themselves are `.github/workflows/ci.yml` and
`.github/workflows/release.yml`. `make ci` runs the same targets in the same
order, so a green local run means a green CI run.

## Documentation layout

`deployment/` is the first folder here to follow the five-file feature-docs
layout — architecture, engineering, CLI, API and user. The rest of `docs/` is
the older topic-folder shape and is unconverted, not exempt: new features get a
feature folder, and the existing folders are migrated one feature at a time
rather than all at once or in passing.
