# GUI — API

> **Status: NORMATIVE** · Scope: the Wails binding and events · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The desktop window talks to the Go process over Wails, not HTTP. The server
build serves the same frontend over HTTP; its endpoints are in
[server/api.md](../server/api.md).

## Bound methods (`MonitorBinding`)

| Method | Returns | Behaviour |
|---|---|---|
| `GetSnapshot()` | snapshot, error | The monitor's latest reading, on demand |
| `GetVersion()` | `map[string]string` | Keys `version`, `gitCommit`, `buildDate` |
| `SetInterval(ms)` | error | Retunes the running monitor in place. `types.InvalidIntervalError` if `ms <= 0`. Unlike `POST /api/interval` it does not restrict `ms` to the dropdown values |
| `Resync()` | nothing | Cancels the current frame stream, waits for it to exit and starts a new one; returns at once |

## Events emitted to the page

| Event | Payload |
|---|---|
| `sysmon:full` | A whole snapshot frame |
| `sysmon:delta` | A patch against the previous frame |

Frame format and sequencing are those of the delta stream
([server/delta-streaming.md](../server/delta-streaming.md)).
