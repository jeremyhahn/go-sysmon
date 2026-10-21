# go-sysmon roadmap

> **Status: REFERENCE** · Owner: Software Department · Last verified: 2026-10-11
> Changes are proposed by docs-only PR.

## Mission

A standalone Linux system monitor: CPU, memory, disk and network, live over SSE.
One collector behind a CLI, a web GUI and a desktop app, embeddable as a library.
Runs as infrastructure and aggregates a fleet of hosts.

## Current milestone

[Release candidate](../../milestone/24); latest release is v0.3.0-alpha.
Target kind: RC; projected date: set by the department plan.

## Now

- [#252](../../issues/252) govulncheck runs at a pinned version — `docs/testing/` — open
- [#253](../../issues/253) NVMe devices report write counters — `docs/server/` — open
- [#255](../../issues/255) roadmap lists the RC milestone, with the docs folder per item — `ROADMAP.md` (no feature folder) — open
- [#257](../../issues/257) network rates display in bits per second — `docs/gui/` — open
- [#258](../../issues/258) network interfaces report NIC vendor and model — `docs/collector/` — open
- [#259](../../issues/259) wifi adapter detail shows in the GUI — `docs/gui/` — open
- [#260](../../issues/260) disks report peak throughput since start — `docs/gui/` — open
- [#261](../../issues/261) server exposes a health endpoint — `docs/server/` — open
- [#262](../../issues/262) forwarded headers honoured only from trusted proxies — `docs/server/` — open
- [#243](../../issues/243) CI workflows run without the host Docker socket — `docs/testing/` — blocked: go-xkms#1332 needs an owner ruling
- [#256](../../issues/256) feature docs pass the spec-review scrub — `docs/` (every feature folder) — blocked: department slot on dept-software#126

## Next

Nothing queued. New work arrives as issues.

## Later

- [#244](../../issues/244) release artifacts are signed — deferred by owner ruling, off the RC milestone

## Dependencies

- Up: none; no in-house modules are pinned in go.mod.
- Down: the fleet hosts it monitors; planned deployment into go-trusted-platform.

## Non-goals

- Anything outside Linux system monitoring; see [ownership and SDK boundary](../handbook/software/docs/00-ownership-and-sdk-boundary.md).
