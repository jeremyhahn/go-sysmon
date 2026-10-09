# GUI — implementation contract

> **Status: NORMATIVE** · Scope: `cmd/sysmon/gui_*.go`, `bindings.go`, `emit_*.go`, `tray_*.go`, `cmd/sysmon/frontend/` · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

Design, lifecycle, asset embedding and frame validation are in
[architecture.md](architecture.md); visual rules in [house-style.md](house-style.md).

## Exported surface

`package main` exports nothing to Go callers. The surface the frontend calls is
the Wails binding `MonitorBinding` (`NewMonitorBinding(mon *monitor.Monitor)`),
listed in [api.md](api.md). Other pieces:

| Piece | Contract |
|---|---|
| `launchGUI() error` | `desktop` build: builds a `SystemCollector` and a monitor at `defaultInterval`, binds `MonitorBinding`, runs Wails (title "System Monitor", 1400x900). Other builds: returns `types.GUIUnavailableError` |
| `startSystray` / `stopSystray` / `trayRunning` | Tray lifecycle; started in `OnStartup` unless `--no-tray`; stubs in builds without `systray` |
| Event emitter (`emit_desktop.go` / `emit_stub.go`) | Wails events in the desktop build; no-op otherwise |

## Invariants

- GUI code compiles only under the `desktop` tag; the server build links no GTK
  or WebKit.
- `SetInterval` retunes the running monitor in place and never replaces it, so
  retuning does not discard accumulated readings (#97).
- Closing the window hides it while the tray is running (`trayRunning()`), and
  quits otherwise.
- `OnShutdown` stops the tray before the binding stops the monitor.
- `Resync` cancels the current stream, waits for it to exit, then starts a new
  one; the call returns immediately.
- The frontend and the server share one bundle. `dist/` is committed;
  `make check-bundle-current` fails when it does not match the sources.
- `src/lib/types.generated.ts` and `schema.generated.ts` are generated from
  `pkg/types/types.go` (`make generate`); never edit them.
- CSP-safe: no inline script or style (see
  [server/architecture.md](../server/architecture.md#security-headers)).

## Error semantics

| Error | When |
|---|---|
| `types.GUIUnavailableError` | `launchGUI` in a non-desktop build; the root command falls back to `overview` |
| `types.InvalidIntervalError` | `MonitorBinding.SetInterval` with `ms <= 0` |
| Frame refused by the schema | The page names the path (`networks[3].flags: expected array, got string`) and escalates a systematic refusal to a client/server version mismatch message |

## Configuration

No GUI configuration file. Flags: `--no-tray` ([cli.md](cli.md)). The window
polls at `defaultInterval` until the dropdown changes it.

## Test strategy

- Go: `bindings_test.go`, `gui_stub_test.go`, `tray_stub_test.go`,
  `tray_desktop_test.go` (`make test-cmd-sysmon`, which runs every build shape).
- Frontend: `svelte-check` (`npm run check` in `cmd/sysmon/frontend`) and
  Playwright specs (`npm run test:e2e`, `make test-e2e`) against a real browser,
  including `tests/csp.spec.ts`.
- Extensions and desktop tiers: [testing/desktop.md](../testing/desktop.md).

## Observability

The GUI logs through the process `slog` default (see the `--log-*` flags).
