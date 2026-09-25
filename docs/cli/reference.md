# CLI reference

Every command takes `--json` and `--refresh`. Run `sysmon` with no arguments
and it picks a mode for you; see [automatic mode selection](#automatic-mode-selection)
at the bottom.

## Global flags

Logging flags apply to every subcommand. Diagnostics go to stderr, which is
what keeps `--json` output on stdout clean enough to pipe into `jq`.

| Flag | Default | Description |
|---|---|---|
| `--log-file` | *(none)* | Write logs to this path instead of stderr; parent directories are created and the file is appended to |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--log-format` | `text` | `text` or `json` |


| Flag        | Type    | Default | Description                                     |
|-------------|---------|---------|-------------------------------------------------|
| `--json`    | bool    | false   | Output as JSON instead of formatted text         |
| `--refresh` | float64 | 0       | Refresh interval in seconds (0 = one-shot)       |
| `--no-tray` | bool    | false   | Disable the system tray icon (desktop build only)|

## Commands

### overview

Print a full system summary: host, CPU, memory, storage, GPU, network, processes, thermal, fans, and PSI.

```bash
sysmon overview
sysmon --json overview          # JSON output
sysmon --refresh 1 overview     # refresh every second
```

This is the default command when no subcommand is given and no display server is detected.

### cpu

Model, topology, frequency range, cache, microcode, and a usage bar per core
with its temperature and voltage alongside. Below that, RAPL package power and
the thermal throttle counters.

Throttle counters are cumulative since boot, so a non-zero value means the CPU
has thermally throttled at some point — not that it is throttling now. That is
usually the question you are actually asking after a slow benchmark.

The temperature table lists each reading's package (`Pkg`, numbered from 0 as
the kernel does), core number and label. Every package numbers its cores from
0, so on a host with more than one package the label names the package too:

```
CPU Temperatures:
   Pkg  Core  Label              Temp      High      Crit
     0     0  P0 · Core 0       50.0°C         -         -
     1     0  P1 · Core 0       60.0°C         -         -
```

A single-package host keeps the driver's plain label, `Core 0`. The dashboard
names cores the same way.

```bash
sysmon cpu
sysmon --refresh 0.5 cpu       # twice a second
```

### memory

RAM and swap, the buffer/cache/slab breakdown, and a table of the physical
modules: manufacturer, part number, type, speed, rank, voltage.

Per-DIMM temperature needs the `spd5118` or `jc42` driver bound to the SMBus,
which most systems do not do by default, and which go-sysmon never does itself
([how to enable it](../collector/sensors.md#dimm-temperatures)). The column
stays empty when it is not available rather than reporting zero.

```bash
sysmon memory
```

### storage

Every disk with its model, capacity, usage, and health: SMART status, NVMe wear
level and remaining spare, estimated life left, temperature, power-on hours,
total data read and written, media errors. Then the partition layout.

Reading SMART needs root or membership of the `disk` group. Without it the
health columns are empty and `sysmon doctor` says so. Note that the block device
node is often readable when the SMART ioctl still is not, so "I can `cat` it"
does not mean this will work.

```bash
sysmon storage
```

### network

Every interface: its kind, MAC, addresses, link speed, duplex, driver, MTU,
traffic counters and error/drop counts. Bridges list their ports, bonds list
their slaves, and wifi adapters carry signal and noise.

The state is rendered as two values, like `[UP/down]`. The first is the
administrative flag, the second is what the link is actually doing. A NIC with
no cable is up and down at the same time, and conflating the two is a reliable
way to misdiagnose a dead port.

```bash
sysmon network
```

### gpu

Driver and VBIOS versions, compute mode, performance state, PCI topology,
utilisation bars for GPU/memory/encoder/decoder, memory, temperatures, fan,
power draw against its limit, clocks, PCIe throughput and ECC counters.

NVIDIA cards go through NVML and report all of the above. AMD and Intel come
from sysfs and report considerably less — the vendors simply do not expose as
much there.

```bash
sysmon gpu
```

### containers

Display containers running on the host with per-container CPU, memory, PID and
I/O accounting read from cgroups. No container runtime socket is used, so this
works without access to the Docker or containerd daemon.

`CPU` is a percentage of a single core, matching the `docker stats` convention:
a container saturating two cores reports 200%.

The table includes the CPU quota, throttling percentage, peak memory and
uptime. Below it, an **Attention** block lists any container that is throttled,
has been OOM-killed, peaked at its memory limit, is under stall pressure, or is
swapping — the conditions that explain degraded behaviour.

The first line says whether the node has a container runtime at all, the same
answer that decides whether the dashboard shows its Containers tab (see
[runtime and hypervisor detection](#runtime-and-hypervisor-detection)):

```
Runtime:    present (containerd, docker)
```

`--json` adds it as `container_runtime`, beside `cgroup_version`, `runtimes`
and `containers`.

```bash
sysmon containers
sysmon containers --json
sysmon containers --index 2
```

| Flag      | Type | Default | Description                                        |
|-----------|------|---------|----------------------------------------------------|
| `--index` | int  | `-1`    | Container index to display; omit to show all       |

### vms

Display virtual machines running on the host, described from the hypervisor
process: guest name, UUID, vCPUs, configured and resident memory, and the host
NICs and disk images backing the guest.

Memory is reported twice: the guest's configured RAM and the hypervisor's
resident set on the host, which is normally lower because guest memory is
faulted in lazily. Guest-internal metrics are not reported.

The first line says whether the node has a VM stack at all, and `--json` adds
it as `hypervisor` beside `vms`:

```
Hypervisor: unknown — /proc is mounted hidepid=2, which hides other users' processes from sysmon
```

```bash
sysmon vms
sysmon vms --json
sysmon vms --index 0
```

| Flag      | Type | Default | Description                                 |
|-----------|------|---------|---------------------------------------------|
| `--index` | int  | `-1`    | VM index to display; omit to show all       |

### images

Display container images, their sizes, how much space can be reclaimed, and
where the runtime stores them on disk.

This is the only command that contacts the container runtime's API socket:
image inventory exists only in the runtime's database and its storage directory
is root-owned, so there is no unprivileged filesystem route. Running the command
is the opt-in; no other command talks to the daemon.

It prints the same `Runtime:` line as `containers`. `--json` is the runtime's
fields at the top level, as before, with `container_runtime` added beside them.

```bash
sysmon images
sysmon images --json
```

The query walks every layer and volume and takes several seconds on a host with
a large image library. If the runtime rejects the query, the command exits with
that error instead of listing images with zero sizes.

| Flag | Default | Description |
|---|---|---|
| `--sort` | `size` | `size`, `shared`, `tag`, `id`, `created` or `used`. Numeric keys order largest-first, which is the useful end when reclaiming space |
| `--filter` | *(none)* | Show only images whose tag or ID contains this text (case-insensitive) |
| `--unused` | `false` | Show only images no container references — exactly what a prune would remove |
| `--limit` | `0` | Show at most this many images (0 = all) |

Anything hidden by these flags is reported below the table, so a filtered view
is never mistaken for the whole inventory. The summary totals always cover
every image regardless of filtering.

```bash
sysmon images --unused --sort size --limit 10   # the 10 biggest reclaimable images
sysmon images --filter grafana                  # one repository
sysmon images --sort created                    # newest first
```

### Runtime and hypervisor detection

`containers`, `images` and `vms` report the detection the API serves as
`virtualization.container_runtime` and `virtualization.hypervisor`. In `--json`
the field is that wire type, unchanged: `state`, `evidence` and `reason`. In
text it is one line:

| State | Line |
|---|---|
| `present` | `present (stack, stack)`, each stack found once, in evidence order |
| `absent` | `absent`: every place the software could be was checked |
| `unknown` | `unknown — reason` |

A snapshot from a build without detection prints
`unknown — not reported by this build`; it is not a claim of absence. What
counts as evidence, and when absence is proved, is described in
[docs/collector/virtualization.md](../collector/virtualization.md#is-there-a-runtime-or-a-hypervisor-at-all).

### doctor

Report what this build can do on this machine and how to fix what it cannot:
desktop GUI libraries, SMART readability, cgroup version and container runtime
access. Nothing is installed or changed.

```bash
sysmon doctor
sysmon doctor --json
```

Each check reports `ok`, `degraded` or `unavailable`, and names a remedy **when
there is one to name**. A check that cannot establish what would fix it says
nothing rather than guessing — advice that does not apply is worse than none,
because it sends somebody to change a thing that changes nothing.

**The SMART check's denominator is devices that could ever answer.** A
device-mapper node has no medium and a drive that reports it implements no SMART
never will, so both are excluded and counted separately as devices that cannot
answer. Counting them made a perfect score unreachable on a healthy machine, and
a check nobody can pass is a check people learn to ignore.

A device behind a RAID controller **is** counted. That status means the device
did not answer directly; it does not establish that the device is a volume
rather than a passthrough disk, and excluding it would let a disk that genuinely
fails to answer disappear from the count.

**And the drives behind that controller are counted too.** They are not block
devices and appear in no disk list, so a figure summed over the list alone
reported ci-node-1 as reading four devices while ten answered. The detail line
says how many came from a controller — `readable on 10 of 12 device(s) that can
report it (6 behind a RAID controller)` — because otherwise a count larger than
the number of disks on the page reads as an error rather than as drives nobody
could see before.

**The remedy comes from the device, not from a constant.** A refused *open* is
file permissions and the `disk` group fixes it; a refused *passthrough* is a
capability check that no group membership satisfies. The check used to offer the
group for both, which on a host already in that group was advice to do something
already done. When several devices fail for different reasons, the most common
is shown and the rest are counted — the reasons carry device paths and errno
detail, so printing them all would put a paragraph on one line.

A host where nothing can report SMART is `unavailable`, not `degraded`, and
offers no remedy: there is nothing to fix.

### serve

Start the HTTP server for real-time browser-based monitoring, streaming
snapshots as server-sent events.

```bash
sysmon serve --addr :8080 --interval 1000
```

| Flag         | Type   | Default | Description                         |
|--------------|--------|---------|-------------------------------------|
| `--addr`     | string | `:8080` | TCP listen address                  |
| `--interval` | int    | `1000`  | Polling interval in milliseconds    |

### version

Print the build version, git commit, and build date.

```bash
sysmon version
```

## Automatic mode selection

Run `sysmon` with no subcommand and it decides:

1. If a display server is present (`DISPLAY` or `WAYLAND_DISPLAY` is set) *and*
   the binary was built with the `desktop` tag, the GUI window opens.
2. Otherwise you get `overview` as a one-shot.

The server build always takes the second path, since it has no GUI compiled in.

## One-shot mode

Without `--refresh`, a command takes two readings 500 ms apart
(`oneShotSampleInterval` in `cmd/sysmon/snapshot.go`) and prints the second,
the way `top -n1` and `mpstat` do. CPU usage and every other rate — network,
disk, swap paging — is the difference of two readings, so a single reading
could never show one. A figure the second reading still could not measure,
such as a core whose `/proc/stat` line was malformed, prints as `-`, never as
0.

## Refresh mode

`--refresh N` takes seconds, decimals allowed. The terminal clears between
frames, and tiering is disabled so every metric is genuinely re-read rather
than served from cache. Ctrl+C to stop.

Sleep time is the interval minus however long collection and rendering took, so
`--refresh 1` means a frame per second rather than a frame per second-plus-work.
The first frame has no earlier reading to measure rates against, so CPU usage
reads `-` until the second.
