# GUI — CLI

> **Status: NORMATIVE** · Scope: how the GUI is started · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The GUI has no verb of its own. It is the default action of the root command.

| Invocation | Result |
|---|---|
| `sysmon` | Window opens if `DISPLAY` or `WAYLAND_DISPLAY` is set and the binary has the `desktop` tag; otherwise `overview` |
| `sysmon --no-tray` | The same, without the tray icon (closing the window then quits) |
| `sysmon serve` | The dashboard in a browser instead ([server/cli.md](../server/cli.md)) |

`--no-tray` is a global flag (bool, default `false`) and only matters in the
desktop build. `sysmon-server` never opens a window.

Build and dev targets:

| Target | What it does |
|---|---|
| `make build-desktop` | The desktop binary, `bin/sysmon` |
| `make build-frontend` | The frontend bundle only |
| `make dev` | Wails dev mode with hot reload |
| `make deps` | Installs the GTK/WebKit build dependencies |

In `cmd/sysmon/frontend`: `npm run dev` (Vite), `npm run build`, `npm run check`,
`npm run test:e2e`.

```bash
make build-desktop && bin/sysmon --no-tray
```
