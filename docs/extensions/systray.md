# System tray

Live metrics in the notification area, and a way back to the window after you
have closed it.

## Building it

The tray compiles when the `systray` tag is present, which `make build`
includes by default. To leave it out:

```bash
make build WITH_SYSTRAY=0
```

| File              | Tag        | Purpose                     |
|-------------------|------------|-----------------------------|
| `tray_desktop.go` | `desktop`  | The real implementation     |
| `tray_stub.go`    | `!desktop` | No-ops                      |

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

## How it works

1. `startSystray` runs the tray in a goroutine via `fyne.io/systray`.
2. It subscribes to the monitor.
3. Each snapshot triggers `updateTrayItems`, which rewrites every menu title.
4. "Open Monitor" calls the Wails `WindowShow`.
5. "Quit" calls the Wails `Quit`.
6. On shutdown, `stopSystray` calls `systray.Quit()`.
