# Collector — API

> **Status: NORMATIVE** · Scope: `pkg/collector` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

No network API: the collector exposes no endpoint, RPC or frame. It produces
the `types.Snapshot` ([types/reference.md](../types/reference.md)) that the
monitor publishes and the server serves ([server/api.md](../server/api.md)).
Its Go surface is in [engineering.md](engineering.md#exported-surface).
