# GUI — using it

> **Status: NORMATIVE** · Scope: the desktop app and the web dashboard · Last verified: 2026-10-09
> Conflicts with this document are defects. Fix them here first.

The desktop window and the browser dashboard show the same pages. Use the
desktop app on a workstation, the browser on any other machine that runs
`sysmon serve`.

## Prerequisites

- Desktop: the desktop build (`bin/sysmon`) and GTK 3, WebKit2GTK and libsoup 3
  installed; `sysmon doctor` names what is missing and the install command.
- Browser: a running `sysmon serve` ([server/user.md](../server/user.md)).

## First run

```bash
sysmon                         # desktop window (needs a display)
sysmon serve --addr :8080      # then open http://localhost:8080
```

## Common tasks

- **Change the refresh rate.** Use the interval dropdown. In the browser it
  applies to everyone viewing that server.
- **Find a subsystem.** The page has a panel per area: host, CPU, memory,
  disks, network, GPU and NPU where present, processes, and containers and VMs
  where the node has them.
- **Switch light/dark.** The theme toggle.
- **See alerts.** The bell and banner show active threshold alerts. Thresholds
  are set in the config file; the settings dialog shows what is in force.
- **Look back in time.** The date-range picker appears only when the node has
  a history store configured ([server/alerting-fleet-config.md](../server/alerting-fleet-config.md)).
- **Keep the app in the tray.** Closing the window hides it while the tray icon
  runs; use the tray menu to reopen or quit. With `--no-tray`, closing quits.

## Troubleshooting

- **`sysmon` prints an overview instead of opening a window.** No display was
  found, or this is the server build.
- **Loader error about `libglib`/`libwebkit2gtk`.** The desktop build needs the
  GTK stack; install it or use `sysmon-server`.
- **The page shows "out of step".** The page and server are different versions;
  reload, and make sure the server was rebuilt with the frontend.
- **Numbers look frozen.** Check the interval; at 60 s the page updates once a
  minute.
- **A tab or panel is missing.** It is shown only when the node has that
  hardware, runtime or history.
