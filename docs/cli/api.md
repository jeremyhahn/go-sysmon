# CLI — API

> **Status: NORMATIVE** · Scope: `cmd/sysmon` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

No API of its own. The CLI exposes no endpoint, RPC or frame; its programmatic
surface is the `--json` output of each verb, which is the `types.Snapshot`
(documented in [types/reference.md](../types/reference.md)), plus the
`container_runtime`, `hypervisor` and image fields described in
[reference.md](reference.md).

`sysmon serve` starts the HTTP API, documented in [server/api.md](../server/api.md).
