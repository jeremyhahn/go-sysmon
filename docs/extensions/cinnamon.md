# Cinnamon panel applet

Per-core bars and gauges drawn straight onto the Cinnamon panel, with the full
data set one click away, fed by the sysmon event stream.

## What you get

### On the panel

Cairo rendering, no widgets:

- CPU — one thin vertical bar per core
- Memory — a single wider bar
- Disk — aggregate usage across all disks
- Network — throughput as compact text

Bars run green under 50%, yellow to 80%, red above.

The per-core bars are the reason this exists. A row of 24 thin bars tells you at
a glance whether load is spread or pinned to one core, which a single average
number cannot.

### In the popup

A panel applet is about twenty pixels tall, so everything else lives in the
menu. Clicking the applet opens a collapsible section per subsystem, carrying
the same information the CLI and the web dashboard show:

| Section         | What is in it                                                        |
|-----------------|----------------------------------------------------------------------|
| Host            | Hostname, OS, kernel, uptime, board and BIOS                         |
| CPU             | Model, topology, cache, load average, per-core usage/frequency/temp, flags |
| GPU             | Per device: driver, utilisation, memory, thermals, power, perf state  |
| Memory          | Used/free/buffers/cached/slab, swap, and per-DIMM part numbers and speeds |
| Disks           | Per device: identity, filesystem usage, I/O rates and totals, queue depth, utilisation, peaks, SMART and NVMe health, partitions |
| Network         | Per interface: kind, state, addresses, MTU, DNS, throughput, counters, errors, drops, bridge/bond membership, wireless details |
| Sensors         | Core temperatures, package power, thermal zones, fans, throttle counts |
| Processes       | State counts and the ten busiest processes                            |
| Virtualisation  | Runtime details, containers, virtual machines, capability notes       |

Absent readings render as `-` rather than a zero, so a field the collector
could not read is visibly different from one that genuinely measured nothing.

## Requirements

- Cinnamon 5.4 through 6.4
- A running server: `sysmon serve`

## Installing

```bash
make install-extension-cinnamon
```

That copies the applet and the shared modules to
`~/.local/share/cinnamon/applets/sysmon-go@sysmon/`. Then right-click the panel,
choose Applets, find "System Monitor (go-sysmon)" and add it.

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

The gauge toggles only affect drawing, so changing one repaints the panel.
Changing the server address reconnects.

## How it works

1. On init it opens `GET http://<server-address>/api/events`, a server-sent
   events stream.
2. It `POST`s the configured rate to `/api/interval`.
3. Each `snapshot` event repaints the Cairo drawing area, updates the tooltip
   and refreshes the popup sections.
4. The drawing area sizes itself to whichever gauges are enabled.
5. Left-clicking opens the popup; the last item launches the `sysmon` GUI.

GJS has no `EventSource`, so the applet reads the response body itself through
libsoup and feeds each line to the parser in `sse.js`.

### Reconnecting

If the stream drops — server restart, suspend, network blip — the applet
reconnects, starting from the delay the server advertised in its `retry` field
and doubling up to 30 seconds. The backoff resets when data actually arrives,
not merely when a connection is accepted: a server that accepts and immediately
closes would otherwise be retried every three seconds forever.

Only one reconnect timer runs at a time. An `event: bye` means the monitor
stopped deliberately, and the applet says so rather than pretending the link is
merely flaky.

When you remove the applet from the panel it cancels any pending timer and
closes the stream, rather than leaving the server holding a subscriber slot.

## Layout

```
extensions/cinnamon/sysmon-go@sysmon/
    applet.js              GObject Introspection glue: libsoup, St, PopupMenu
    metadata.json          UUID, version, Cinnamon compatibility
    settings-schema.json   The settings above

extensions/shared/         installed alongside applet.js
    format.js              Byte, rate, temperature and duration formatting
    snapshot.js            Snapshot parsing and aggregation
    sse.js                 Server-sent events parser
    sections.js            The popup's section model

extensions/tests/          Node unit tests over the above
    harness/cinnamon.js    Stand-in Cinnamon platform for loading applet.js
```

Everything that can be tested without a desktop lives in `extensions/shared/`.
`applet.js` holds only the platform glue, which is what keeps the parity model
under test.

## Testing

Three tiers. See [testing/desktop.md](../testing/desktop.md) for the full
picture.

```bash
make test-extensions          # tiers 1 and 2: fast, hermetic, part of make ci
make test-extensions-desktop  # tier 3: real Cinnamon in a container, nightly
```

## Working on it

GJS against Cinnamon's applet API. The dependencies that matter:

- `Soup` (libsoup 3) for HTTP, and `Gio.DataInputStream` to read the stream
- `St.DrawingArea` for Cairo
- `imports.ui.popupMenu` for the section menus
- `imports.ui.settings.AppletSettings` for settings binding

Cinnamon's `require()` resolves against the applet directory and **rejects a
path containing a subdirectory** — `require("./lib/format.js")` fails with
"Path does not exist" pointing at the applet root. That is why the shared
modules are installed flat next to `applet.js`. They use CommonJS exports, the
same form Node needs to test them. The tier 2 harness enforces the same
restriction, so a subdirectory import fails in CI rather than on a panel.

Edit in place, reinstall with `make install-extension-cinnamon`, then restart
Cinnamon: Alt+F2, type `r`, Enter. There is no hot reload, and a syntax error
shows up in `~/.xsession-errors` rather than anywhere obvious. Run
`make test-extensions` first — it catches that class of mistake without a
restart.
