# Disk and network detail

Reference for the debugging-oriented fields on `DiskInfo` and `NetworkInfo`.

## Disk I/O, queue depth and peaks

### Rates

`read_bytes_rate` and `write_bytes_rate` are **bytes per second**, computed
from the counter delta divided by elapsed wall-clock time. They are not
per-polling-interval deltas: the stream rate is user-selectable from 500ms to
60s, and a raw delta would silently change meaning whenever that dropdown
moved.

A device seen for the first time reports `0` — there is no previous sample to
subtract, and reporting its lifetime total as one interval's rate would show a
phantom spike at startup.

A counter that goes backwards (device re-enumerated) yields `0` rather than a
enormous bogus rate.

### Queue depth

| Field | Meaning |
|---|---|
| `queue_length` | Instantaneous in-flight requests, from `/sys/block/<dev>/inflight` (reads + writes) |
| `avg_queue_length` | Average queue over the sample window, from the `weighted_io` delta. Equivalent to `iostat` **avgqu-sz** |

A high `avg_queue_length` with a low `util_percent` points at a device
servicing requests slowly rather than one that is saturated.

### Utilisation

`util_percent` is the share of wall-clock time the device spent servicing I/O,
from the `io_time` delta. This is `iostat`'s **%util**. It is clamped to 100
because a multi-queue device can report more busy time than elapsed time.

### Peaks

Peaks are tracked per device for the lifetime of the process:

- `peak_read_bytes_rate`, `peak_write_bytes_rate`
- `peak_queue_length`
- `peak_util_percent`
- `peak_used_percent`

They exist because a point-in-time sample lands *between* bursts. A disk that
briefly saturated at 2 GB/s an hour ago looks idle in the current reading; the
peak is what tells you it happened.

Note that disks are collected on the **slow tier** (every 30 ticks), so at the
default 1s interval a rate needs roughly a minute of uptime before two samples
exist.

## Network classification

### Kind

`kind` classifies each interface from sysfs, and is more specific than the
`is_virtual` flag:

| Kind | Detected by |
|---|---|
| `loopback` | interface named `lo` |
| `bridge` | `bridge/` directory |
| `bond` | `bonding/` directory |
| `wifi` | `wireless/` directory or `phy80211` |
| `tun` | `tun_flags` |
| `veth` | `veth` name prefix |
| `vlan` | `lower_*` link or a dot in the name |
| `ethernet` | has a `device/` directory |
| `virtual` | none of the above |

The most specific test wins: a bridge is also virtual, and a wifi adapter is
also ethernet-like.

### Operational state

`is_up` is the administrative `IFF_UP` flag; `oper_state` is what the link is
actually doing. A NIC with no cable attached is `is_up: true` with
`oper_state: "down"`, which the CLI renders as `[UP/down]`. Treating `is_up`
alone as "working" is a common source of confusion.

### Topology

| Field | Populated when |
|---|---|
| `bridge_ports` | the interface is a bridge — lists its member ports |
| `bond_mode`, `bond_slaves` | the interface is a bond master |
| `bond_master` | the interface is enslaved to a bridge or bond |

This is what connects a `veth` device to the bridge carrying its traffic, and
therefore to the container behind it.

### Resolver

`dns_servers` and `dns_search` come from systemd-resolved's per-link state
(`/run/systemd/resolve/netif/<ifindex>`) when it exists. Otherwise the
system-wide configuration is reported, since that is what the interface's
traffic will actually resolve through.

`/run/systemd/resolve/resolv.conf` is preferred over `/etc/resolv.conf`,
because the latter usually points at the `127.0.0.53` stub rather than the real
upstream servers.

A bare `.` search domain (systemd's way of writing the root domain) is
filtered out.

### Wireless

For `wifi` interfaces, `wireless` carries link quality, signal level and noise
in dBm from `/proc/net/wireless`. It is `null` when the interface has no entry,
which is the case for a radio that is down.

SSID and BSSID require an nl80211 netlink query and are not reported.
