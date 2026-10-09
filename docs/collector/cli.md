# Collector — CLI

> **Status: NORMATIVE** · Scope: `pkg/collector` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

No CLI surface of its own: the collector is a library. Its data is shown by the
`sysmon` verbs (`cpu`, `memory`, `storage`, `network`, `gpu`, `npu`,
`containers`, `vms`, `images`, `host`, `overview`) and checked by
`sysmon doctor`; all are in [cli/cli.md](../cli/cli.md).
