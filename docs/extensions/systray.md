# System tray

> **Status: REFERENCE** · Last verified: 2026-10-09
> As-built description of the system tray extension.

Live metrics in the notification area, and a way back to the window after you
have closed it.

## Building it

The tray compiles when the `systray` tag is present, which `make build`
includes by default. To leave it out:

```bash
make build WITH_SYSTRAY=0
```

| File              | Constraint             | Purpose                 |
|-------------------|------------------------|-------------------------|
| `tray_desktop.go` | `desktop && systray`   | The real implementation |
| `tray_stub.go`    | `!desktop \|\| !systray` | No-ops                  |

Without the tray, closing the window quits the application; with it, closing
hides the window and **Open Monitor** brings it back.

To turn it off without rebuilding:

```bash
sysmon --no-tray
```

## The menu

| Item          | Content                          | Example                            |
|---------------|----------------------------------|------------------------------------|
| CPU           | Average across all cores         | `CPU:  42.3%`                      |
| RAM           | Used / total, with percent       | `RAM:  12.4 GB / 31.2 GB (39.7%)`  |
| Swap          | Used / total, with percent       | `Swap: 0 B / 8.0 GB (0.0%)`        |
| Load          | 1m / 5m / 15m                    | `Load: 1.23 / 0.98 / 0.87`         |
| GPU           | Utilisation and memory, per GPU  | `GPU0: 65.0%  mem 43.2%`           |

GPU rows appear when the first snapshot containing GPU data arrives, so a
machine without one gets no empty placeholder.

Below the numbers: **Open Monitor** brings the window back, **Quit** exits
everything.

## Closing the window

With the tray running, closing the window hides it rather than quitting. That
is usually what you want — the tray keeps updating, and the window is one click
away. Without the tray (`--no-tray`), closing the window exits normally.

## The icon

Drawn at runtime in pure Go: a 22x22 PNG, green monitor outline, grey stand. No
icon files to install, nothing to go missing when the binary is copied to
another machine.

If the PNG encoder rejects the drawn image, the failure is logged as a warning
carrying a `TrayIconEncodeError`, and the tray shows `cmd/sysmon/trayicon.png`
instead, which is compiled into the binary. That file is the same icon, pixel
for pixel, and a test fails if the drawing and the file ever differ. So an
encode failure never shows a blank or truncated icon, and it never happens
silently.

The menu text uses the CLI's own formatters (`cli.FormatBytes`,
`cli.FormatPercent`, `cli.FormatMeanCPUUsage`), so the tray and `sysmon memory`
print the same value the same way. CPU reads `-` until a core's usage has been
measured, which takes the monitor's second collection.

## How it works

1. `startSystray` runs the tray in a goroutine via `fyne.io/systray`.
2. It subscribes to the monitor.
3. Each snapshot triggers `trayMenu.update`, which rewrites every menu title
   and adds an item for each GPU the first time it is reported.
4. "Open Monitor" calls the Wails `WindowShow`.
5. "Quit" calls the Wails `Quit`.
6. On shutdown, `stopSystray` calls `systray.Quit()`.
