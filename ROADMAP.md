# go-sysmon roadmap

> **Status: REFERENCE** · Owner: Software Department · Last verified: 2026-10-08
> Changes are proposed by docs-only PR.

## Mission

A standalone Linux system monitor: CPU, memory, disk and network, live over SSE.
One collector behind a CLI, a web GUI and a desktop app, embeddable as a library.
Runs as infrastructure and aggregates a fleet of hosts.

## Current milestone

None open in Gitea; latest release is v0.3.0-alpha.
Target kind: RC; no projected date until a milestone is set.

## Now

- [#243](../../issues/243) CI workflows run without the host Docker socket — open
- [#244](../../issues/244) release artifacts are signed — open
- [#245](../../issues/245) feature docs complete, with banners and stamps — open

## Next

Nothing queued. New work arrives as issues.

## Later

Nothing accepted. New work arrives as issues.

## Dependencies

- Up: none; no in-house modules are pinned in go.mod.
- Down: the fleet hosts it monitors; planned deployment into go-trusted-platform.

## Non-goals

- Anything outside Linux system monitoring; see [ownership and SDK boundary](../handbook/software/docs/00-ownership-and-sdk-boundary.md).
