# Monitor — API

> **Status: NORMATIVE** · Scope: `pkg/monitor` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

No network API of its own. The monitor's Go surface is listed in
[engineering.md](engineering.md#exported-surface). It reaches clients only
through the server: `GET /api/snapshot`, `GET /api/events` and
`GET`/`POST /api/interval` in [server/api.md](../server/api.md).
