# Cinnamon panel applet

Per-core bars and gauges drawn straight onto the Cinnamon panel, fed by the
sysmon WebSocket server.

## What you get

Cairo rendering, no widgets:

- CPU — one thin vertical bar per core
- Memory — a single wider bar
- Disk — aggregate usage across all disks
- Network — throughput as compact text

Bars run green under 50%, yellow to 80%, red above.

The per-core bars are the reason this exists. A row of 24 thin bars tells you
at a glance whether load is spread or pinned to one core, which a single
average number cannot.

## Requirements

- Cinnamon 5.4 through 6.4
- A running server: `sysmon serve`

## Installing

```bash
make install-extension-cinnamon
```

That copies the applet to
`~/.local/share/cinnamon/applets/sysmon-go@sysmon/`. Then right-click the
panel, choose Applets, find "System Monitor (go-sysmon)" and add it.

To remove:

```bash
make uninstall-extension-cinnamon
```

## Settings

Right-click the applet and choose Configure.

| Setting         | Type       | Default          | What it does                    |
|-----------------|------------|------------------|---------------------------------|
| Server address  | text       | `localhost:8080` | Where the sysmon server is      |
| Update interval | spinbutton | 1000 ms          | Poll rate, 250 to 60000 ms      |
| Show CPU        | toggle     | on               | Per-core bars                   |
| Show memory     | toggle     | on               | Memory bar                      |
| Show network    | toggle     | on               | Throughput text                 |
| Show disk       | toggle     | on               | Disk usage bar                  |

Changing the update interval changes it for every client connected to that
server, not just this applet — there is one poll loop behind all of them.

## How it works

1. On init, it opens a WebSocket to `ws://<server-address>/ws`.
2. It sends a `set_interval` matching the configured rate.
3. Each snapshot repaints the Cairo drawing area.
4. The drawing area sizes itself to whichever gauges are enabled.
5. Hovering shows a tooltip with CPU and RAM percentages.
6. Left-clicking launches the `sysmon` GUI.

If the connection drops (server restart, suspend, network blip) it waits three
seconds and reconnects. Only one reconnect timer runs at a time, so a
server that stays down does not accumulate them.

When you remove the applet from the panel it cancels any pending timer and
closes the socket properly, rather than leaving the server holding a subscriber
slot until the ping timeout.

## Layout

```
extensions/cinnamon/sysmon-go@sysmon/
    applet.js              WebSocket client and Cairo rendering
    metadata.json          UUID, version, Cinnamon compatibility
    settings-schema.json   The settings above

extensions/shared/
    format.js              Formatting helpers
    snapshot.js            Snapshot parsing
```

`extensions/shared/` is shared with any future extension, so byte formatting
cannot drift between the applet and whatever comes next.

## Working on it

GJS against Cinnamon's applet API. The dependencies that matter:

- `Soup` (libsoup 3) for WebSockets
- `St.DrawingArea` for Cairo
- `imports.ui.settings.AppletSettings` for settings binding

Edit in place, reinstall with `make install-extension-cinnamon`, then restart
Cinnamon: Alt+F2, type `r`, Enter. There is no hot reload, and a syntax error
shows up in `~/.xsession-errors` rather than anywhere obvious.
