# go-sysmon roadmap

> **Status: REFERENCE** · Owner: Software Department · Last verified: 2026-10-21
> Changes are proposed by docs-only PR. Each line names its feature in [docs/SPEC.md](docs/SPEC.md).

## Mission

A standalone Linux system monitor: CPU, memory, disk and network, live over SSE.
One collector behind a CLI, a web GUI and a desktop app, embeddable as a library.
Runs as infrastructure and aggregates a fleet of hosts.

## Milestones

- [Release candidate](../../milestone/24) — target kind: RC — projected date: set by the department plan. Latest release is v0.3.0-alpha.

## Complete

- **cpu-metrics** — every CPU with per-core usage, temperatures and flags — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **memory-metrics** — memory, swap and DIMM detail — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **accelerator-metrics** — GPUs and NPUs — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **virtualization** — containers, VMs and images from the host side — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **monitor** — one poll loop broadcasting to every subscriber — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **event-stream** — SSE with a configurable interval and delta streaming — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **tls** — static and per-handshake TLS — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **configuration** — reloadable settings file — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **alerts** — threshold rules evaluated from the configuration — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **fleet** — peer polling, roll-up and aggregator trees — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **cli** — every subsystem from the terminal, text or JSON — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **gui** — desktop app and browser dashboard — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **desktop-extensions** — Cinnamon applet and system tray — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **wire-types** — one set of snapshot types, generated for the dashboard — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **container-image** — Docker and Kubernetes against the host — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **release-pipeline** — changelog fragments and tag-driven releases — [v0.3.0-alpha](../../releases/tag/v0.3.0-alpha) — done
- **disk-metrics** — disks report peak throughput since start — [#260](../../issues/260) — done
- **network-metrics** — network rates display in bits per second — [#257](../../issues/257) — done
- **network-metrics** — network interfaces report NIC vendor and model — [#258](../../issues/258) — done
- **network-metrics** — wifi adapter detail shows in the GUI — [#259](../../issues/259) — done
- **http-api** — server exposes health endpoints — [#261](../../issues/261) — done
- **http-api** — forwarded headers honoured only from trusted proxies — [#262](../../issues/262) — done
- **prometheus-metrics** — NVMe devices report write counters — [#253](../../issues/253) — done
- **remote-deployment** — the watchdog notification never blocks — [#265](../../issues/265) — done

## Now

- **ci-pipeline** — govulncheck runs at a pinned version — [#252](../../issues/252) — done
- **ci-pipeline** — CI runs in a PR tier and a full tier — [#267](../../issues/267) — done
- **ci-pipeline** — CI workflows run without the host Docker socket — [#243](../../issues/243) — blocked: go-xkms#1332 needs an owner ruling
- **project-docs** — roadmap lists the RC milestone — [#255](../../issues/255) — done
- **project-docs** — the project carries docs/SPEC.md and an agreeing ROADMAP.md — [#256](../../issues/256) — open

## Next

Nothing queued. New work arrives as issues.

## Later

- **release-signing** — release artifacts are signed; deferred, off the RC milestone — [#244](../../issues/244) — open

## Future

Nothing accepted beyond Later.

## Dependencies

- Up: none; no in-house modules are pinned in go.mod.
- Down: the fleet hosts it monitors; planned deployment into go-trusted-platform.
