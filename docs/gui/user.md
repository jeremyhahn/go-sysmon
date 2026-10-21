# GUI — using it

> **Status: NORMATIVE** · Scope: the desktop app and the web dashboard · Last verified: 2026-10-11
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
- **Identify a NIC.** Each physical interface in the network panel has a line
  under its MAC and driver naming the manufacturer and model, the bus with
  its `vendor:device` identifiers, and the board maker when it differs from
  the chip's. Virtual interfaces have no such line
  ([collector/disk-and-network.md](../collector/disk-and-network.md#hardware-identity)).
- **Switch light/dark.** The theme toggle.
- **See alerts.** The bell and banner show active threshold alerts. Thresholds
  are set in the config file; the settings dialog shows what is in force.
- **Look back in time.** The date-range picker appears only when the node has
  a history store configured ([server/alerting-fleet-config.md](../server/alerting-fleet-config.md)).
- **Keep the app in the tray.** Closing the window hides it while the tray icon
  runs; use the tray menu to reopen or quit. With `--no-tray`, closing quits.

## Reading the panels

- **Network rates are bits per second.** An interface's send and receive rates,
  and its traffic chart, on the Network tab and the Overview, use decimal bit units as links are rated (bps, Kbps,
  Mbps, Gbps, Tbps; 1 Kbps = 1000 bps). So do a VM's network in and out on the Virtualization tab, a node's
  network figure in the Fleet view, and the fleet network rollup. Byte totals stay in bytes (KB, MB, …), as do
  storage rates: disk, paging, and process and container I/O.
- **An unmeasured network rate reads "unknown".** A rate needs two samples. Until the collector has
  both, an interface's send and receive rates (Network tab, Overview) read "unknown", not "0 bps". A
  measured zero still reads "0 bps".
- **DNS is per interface.** Each interface's card lists the resolvers configured for that interface, and
  its search domains. An interface with none shows no DNS line.
- **Wifi link.** A wifi interface's card shows the driver's link quality
  (higher is better; the scale is the driver's), the signal level and, when the
  driver reports it, the noise floor, both in dBm. Wired interfaces show none.
  An associated interface also shows its SSID and the BSSID of its access point
  ([collector/disk-and-network.md](../collector/disk-and-network.md#wireless)).
- **Disk peaks.** Each disk card shows, beside the current figures, the peaks
  since the server started: utilisation, queue length, and read and write
  throughput (binary bytes per second, as disk rates are elsewhere).

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
