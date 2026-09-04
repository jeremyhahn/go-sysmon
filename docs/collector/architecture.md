# Collector architecture

`pkg/collector` reads system metrics from the kernel — procfs, sysfs, hwmon and
a couple of ioctls — and assembles them into one `types.Snapshot`. There is no
agent and no daemon. Every number in the UI came out of a file the kernel
exposes, or an ioctl issued against a block device.

## Data flow

```
+----------------+     +-------------------+     +-----------+
|  Linux kernel  |     |  SystemCollector  |     |  Monitor  |
|  procfs/sysfs  +---->+  (sub-collectors) +---->+  (poll)   |
|  hwmon / ioctl |     |                   |     |           |
+----------------+     +-------------------+     +-----+-----+
                                                       |
                                           +-----------+-----------+
                                           |           |           |
                                        Server       GUI          CLI
                                         (SSE)     (Wails)     (one-shot)
```

## Sub-collectors

Each one owns a subsystem and implements a single method:

```go
type Collector interface {
    Collect() error
}
```

| Sub-collector      | Source          | What it reads                                     |
|--------------------|-----------------|---------------------------------------------------|
| `HostCollector`    | gopsutil, DMI   | Hostname, OS, kernel, board, BIOS, uptime         |
| `CPUCollector`     | gopsutil, sysfs | Per-core usage, model, topology, flags, frequency |
| `GPUCollector`     | NVML, sysfs     | NVIDIA, Intel and AMD utilisation, memory, thermals |
| `MemoryCollector`  | gopsutil, SMBIOS| RAM and swap usage, buffers, cache, per-DIMM hardware |
| `DiskCollector`    | gopsutil, SMART | Partitions, I/O counters, SMART and NVMe health   |
| `NetworkCollector` | gopsutil, sysfs | Interface detail, traffic counters, rates         |
| `ProcessCollector` | gopsutil        | Per-process CPU, memory and I/O                   |
| `LoadCollector`    | gopsutil        | 1, 5 and 15-minute load averages                  |
| `SensorCollector`  | sysfs, hwmon    | Core temperatures, voltages, fans, RAPL, PSI, zones |
| `VirtCollector`    | cgroups, procfs | Containers and virtual machines                   |
| `RuntimeCollector` | Docker API      | Image inventory (opt-in; see [virtualization](virtualization.md)) |

## Which devices are disks, and which absences are which

`DiskCollector` reports a device only when the kernel has put a medium behind
it — `/sys/block/<dev>/size` is non-zero. Any host with the `nbd` module loaded
gets sixteen empty stubs at boot, and on ci-node-1 they sorted ahead of every
real disk, so the storage panel opened on seventeen rows of zeroes while the
machine was writing 70 MB/s further down. The test is the medium, not a list of
names: on that host `nbd0` still carried lifetime I/O counters from a
disconnected export, so "has it ever done I/O" would have kept the emptiest one.

An unreadable `/sys` reads as *unknown*, not as *empty*, so a host without sysfs
mounted keeps its disk list instead of losing it.

The rule is "devices with media", which is deliberately broader than "unused nbd
stubs": an empty card reader, a drive with no disc, and a device inside a
hotplug or rescan window are also omitted, the last of them for one cycle until
the kernel publishes a size.

SMART is then classified rather than merely attempted, because "no reading"
covers four unrelated situations that used to render identically (see
[`smart_status`](../types/reference.md#diskinfo)):

- **No medium to ask.** Only a device with hardware under it has a `device` link
  in sysfs, which makes this structural rather than a list of name prefixes.
  Device-mapper nodes stay in the disk list — `dm-0` is where `/` lives on
  ci-node-1, and dropping it would take the root filesystem's usage with it.
- **Refused.** Reported first, not because it is the most specific answer but
  because it is a fault in how the service was deployed and it is fixable. The
  two shapes of refusal have different remedies and are reported separately:
  the device node could not be **opened** (file permissions — group
  membership), or the node opened and the kernel rejected the **passthrough**
  (a capability check — `CAP_SYS_RAWIO`, which no group membership satisfies).
  Conflating them produces confident advice that cannot work: on ci-node-1 the
  node opens and the ioctl is refused. The probe below is skipped once we know
  we were refused, since it would be refused identically.
- **Behind a RAID controller** — meaning *served by one and did not answer
  SMART directly*, which is what the evidence supports. The SCSI host driver is
  knowable from `/sys/class/scsi_host/<host>/proc_name`; whether **this** device
  is a volume or a passthrough disk is not, so the reason says "if it is a
  volume" rather than asserting it. This is consulted only for failures that
  are *not* refusals and where **nothing came back at all**. A checksum
  failure, an all-zero sector, a short transfer or a sector that decoded to no
  attributes all mean the command got through and bytes were returned, so the
  controller was not what stood in the way; blaming it would send the reader
  looking for member drives instead of at the device in front of them. Note the
  claim is deliberately no stronger than that — an all-zero payload is what a
  bridge returns when it answers *without asking the drive*, so "bytes came
  back" is knowable and "the drive replied" is not. A JBOD disk that was merely refused would likewise be labelled
  as having member drives it does not have, hiding the fixable fault behind the
  claim. Measured on ci-node-1, the ordering costs nothing: a PERC volume on
  a host that *does* hold `CAP_SYS_RAWIO` fails with `unsupported scsi opcode`,
  a check condition rather than `EPERM`, so this is still reached wherever it is
  the whole truth. When a refusal happens on a RAID controller the controller is
  still named in the reason, because on a volume no capability will produce a
  reading and the operator should know that before granting one.
- **The device says it has none.** Asked with ATA IDENTIFY DEVICE over SG_IO,
  never over `HDIO_DRIVE_CMD`: that path segfaults the service on at least one
  deployed host (#20), and this probe runs only after a read has already failed,
  where a crash would be the worst possible way to report bad news. A response
  whose command-set words are marked invalid yields an error, not a `false` —
  "the drive did not say" must not become "the drive said no".
All of the classification reads world-readable sysfs, which matters most in
exactly the case where the reason is that the service is unprivileged.

## Drives behind a hardware RAID controller

A hardware RAID volume hides its members. The kernel publishes the volume as one
SCSI disk — `sda` on ci-node-1 — and the drives that make it up have no device
node, no name in `/sys/block`, no partitions, no filesystem and no I/O counters:
every field `DiskInfo` is built around. They are reachable only by asking the
controller to carry a command to them.

**The implementation is the 64-bit Linux ABI and is built only for it.** A
32-bit target has an 8-byte `struct iovec`, a differently sized packet, and a
different ioctl number, so `linux && (amd64 || arm64)` guards the passthrough
and a stub reports no members elsewhere. A packet laid out for the wrong ABI is
not a subtle error: the controller acts on whatever it finds at each offset.

`DiskCollector` does that through the MFI passthrough: one ioctl on
`/dev/megaraid_sas_ioctl_node` carrying a frame that names a drive by its device
ID and holds an ordinary SCSI CDB. Whatever the CDB says reaches the drive, so
the SAS members answer SCSI LOG SENSE and the SATA members answer the same ATA
PASS-THROUGH(16) command used for directly attached disks, tunnelled one layer
deeper.

**This needs `CAP_SYS_ADMIN` and `CAP_DAC_OVERRIDE`, and does not need
`CAP_SYS_RAWIO`** (measured on ci-node-1, 2026-09-02; either capability alone
fails). It never touches the SG_IO or HDIO paths that `CAP_SYS_RAWIO` governs —
including the one that crash-loops that host — so reading these drives costs no
change to how the service is deployed.

Note also that `/dev/bus/0` **does not exist**. It is `smartctl`'s synthetic
name for the management node, which is why `smartctl -d megaraid,0 /dev/sda`
fails in a way that reads as "this hardware cannot report".

### What is reported, and where

| surface | what it carries |
|---|---|
| `DiskInfo.Members` | one entry per drive: serial, model, vendor, enclosure and slot, temperature, whether the **controller** considers it in service, and its own SMART status |
| `DiskInfo.RAID` | the volume as an array — level, state, degraded, and the member counts — in the same shape an md array uses, so `pkg/alert`, `pkg/metrics` and the fleet roll-up need no second notion of what a RAID array is. **Sync state is left empty: see below** |
| `DiskInfo.SMARTStatus` | stays `behind_controller`: the volume genuinely has no SMART of its own. The reason now names how many members were read |
| `sysmon_disk_member_*` | **labelled by serial**, not by device ID or slot — see below |
| the fleet roll-up | members count toward `smart_capable` and `smart_unhealthy` |
| `pkg/alert` | a failing member raises **`smart_unhealthy`**, the same rule a failing block device raises, and a hot one raises `disk_temp` on the same thresholds |

`RAIDInfo.Members` is deliberately left empty for a hardware volume. That field
names a *block device* because an md array's components are devices this host
addresses directly; a drive behind a PERC is not one, and widening the field to
fit would leave one name covering two different things.

### Alerting, and why members share the block device's rule names

Reading these drives is only worth doing if a failure is *told* to somebody. A
dashboard the operator has to be already looking at is not that, so member
health is wired into the same alert path as block-device SMART.

**They raise the same rule names deliberately.** A separate `smart_member_*`
rule would mean anyone who has already configured notification on
`smart_unhealthy` hears nothing when a drive in a PERC fails — which is the
silence being fixed, arriving by another route. What differs is the message,
which names the **bay and the serial**, because there is no device path to
print and the operator needs to know which drive to pull.

The alert's de-duplication key is the **physical position**, not the serial —
which is the opposite of the metric label, deliberately. A serial is read from
the drive with a separate command every cycle, and that command fails on exactly
the drive an alert is about: one on its way out. Keying on it would have the ID
flip between the serial and the position as the command succeeded and failed, so
an alert that had been firing for an hour would clear and re-fire as a new fault
on a different drive. The position comes from the controller's configuration, in
the same reply that produced the member list, so it cannot vanish while the
member exists.

A metric has the opposite requirement — it is a long-lived history where a
re-bay must not silently continue one drive's series under another drive's
readings, and a missing serial produces a gap rather than a renamed series — so
it keeps the serial. **Different failure modes, different answers**; the message
names both either way.

A member the controller has taken **out of service** raises `raid_member_offline`
(critical), named. `raid_degraded` does not cover it: the array state and the
member state are separate fields, so a volume reported optimal with an offline
drive is a contradiction that would otherwise pass in silence — and even when
the array does report degraded, that alert cannot say *which* drive, which is
the actionable half.

A member with no reading raises nothing. `SMARTEnabled` gates the check exactly
as it does for a block device: a drive that told us nothing is not a drive
reporting failure, and alerting critical on it would be the same substitution of
an absence for an answer this whole change exists to remove, pointing the other
way.

### Sync state is not reported, and says so

Nothing here asks the controller about rebuild, consistency-check or
background-init progress, so `SyncAction` is empty and `SyncProgressPercent` is
zero — **not `"idle"` and `100`**, which is what a healthy md array reports and
therefore what "nothing wrong" looks like in this struct. A volume in the middle
of a rebuild would have been published, drawn and exported as a *completed* one.

A rebuilding array is one drive from data loss. It is the moment this product
exists for, and claiming it finished is the worst available answer — worse than
saying nothing about a healthy host, which is the defect this same struct was
fixed for elsewhere.

The consumers handle the absence rather than papering over it: `pkg/alert`
raises `raid_resync` only for a non-empty, non-idle action, so **the rule is
honestly silent rather than silently satisfied** — an alert whose input is
hardcoded to the all-clear reads as coverage and provides none. `DiskPanel`
already treats `""` exactly as `idle`. `pkg/metrics` omits
`sysmon_raid_sync_percent` entirely, because a percentage nobody measured cannot
be told from a measured one: `0` reads as a stalled rebuild and `100` as a
finished one, on an array that might be neither.

**A rebuild is still not invisible, and for a measured reason rather than an
assumed one.** A member that is not `MR_PD_STATE_ONLINE` reports `Online: false`
and raises a critical `raid_member_offline` naming the bay. That states what can
be proved — this drive is not in service — instead of guessing at a percentage.

**The same applies to `MismatchCount`, one field over**, and the gate found it
only after the sync fix was written: a source that does not report background
operations does not report their results either, so zero there is *no check was
read*, not *the last check was clean*. Those are opposite claims about a
redundant array and only one of them is reassuring. `sysmon_raid_mismatch_count`
and the panel's "Mismatch" cell are gated on the same signal.

That signal — an empty `SyncAction` — is documented on `RAIDInfo` itself rather
than left as a convention between three call sites, because an SDK consumer
following the old contract would have read `""` as malformed.

Decoding real progress needs more DCMDs and a controller in that state to verify
against; it is follow-up work, not something to approximate.

### Online is not the same question as healthy

`DiskMember.Online` is the controller's verdict — the drive is in service as
part of its array — and it is what `RAIDInfo.ActiveDevices` counts. It is
deliberately **not** "the drive answered a health command", which is wrong in
both directions: a drive the controller has failed can go on answering SMART
perfectly, and a working drive can refuse a command it does not implement. Using
readability would have reported a degraded array as whole and a whole one as
degraded.

Only `MR_PD_STATE_ONLINE` (0x18) counts as in service, because that is the only
physical-drive state measured — all six drives on ci-node-1 report it while
both arrays are optimal. An unrecognised state is reported as *not* in service,
which errs toward saying an array has fewer working drives than it has: the
direction that raises attention rather than suppressing it.

### Identity is read every cycle, not cached

Caching a drive's identity by device ID is the obvious optimisation and the
wrong one. **A device ID names a slot on the controller, not the drive in it.**
A replaced drive would keep reporting under the serial of the drive it replaced
until the process restarted — and since that serial is the Prometheus series
identity, the history would look continuous across a hardware change. That is
exactly the failure the serial label exists to prevent, reintroduced one layer
up. Two extra commands per drive on the slow collection tier is not worth it.

### Why the metric label is the serial

A drive behind a controller has no device name, and the names of the things
around it are not stable. ci-node-1's 2026-08-31 rebuild renamed every PERC and
BOSS device on the machine and swapped two of four NVMe names. **A series keyed
on position silently becomes a different series across such a reboot, which is
worse than a missing one: the history looks continuous and is not.** The serial
is the only identifier that survives, so `sysmon_disk_member_smart_healthy` and
`sysmon_disk_member_temperature_celsius` carry `serial` — with `volume` as a
second label so a query can group an array's members without a join. A drive
that will not give a serial is not exported at all, because two unlabelled
drives would collide into one series.

### Building for a target the passthrough does not support

The implementation is the 64-bit Linux ABI, so it is built only for
`linux && (amd64 || arm64)` and `megaraid_unsupported.go` stands in elsewhere.

**That stub is compiled with `go build -tags nomegaraid ./...`, and the tag
exists for exactly that reason.** `pkg/collector` cannot currently be built for
a 32-bit target at all — `gpu.go` imports NVML, which does not compile there
(#8) — so "the unsupported path builds" was a claim nothing could check, and it
was false: two symbols used by untagged files lived only in the 64-bit ones.
Checking which files a target *selects* is not the same as compiling them.

Anything shared between the two paths therefore belongs in an untagged file.
`megaraid_config.go` holds the pure decoders, the error types they raise, and
the mapping onto `RAIDInfo` for that reason.

### What has no automated coverage

The ioctl path cannot run in CI: it needs the hardware. What **is** covered by
tests is everything either side of it — the packet layout against the offsets a
C compiler produced from the driver's structures, the controller configuration
decoder against **a real 1120-byte reply captured from ci-node-1**, the SCSI
log page and INQUIRY decoders, and the mapping onto `RAIDInfo`. The ioctl itself
was verified by running the built binary on ci-node-1 as the service's own user
and reading its output; a green test suite does not mean that path was exercised.

## Frequency tiering

Metrics do not change at the same rate, and some are far more expensive to read
than others. A SMART ioctl can take tens of milliseconds and wakes a sleeping
disk; CPU usage is a cheap read from `/proc/stat`. Running both at 500ms would
mean spinning up drives twice a second to watch a number that moves once an
hour.

So the collector counts ticks and only runs the expensive collectors on some of
them:

| Tier   | Runs            | Collectors                          |
|--------|-----------------|-------------------------------------|
| Fast   | Every tick      | Host, CPU, memory, network, load    |
| Medium | Every 5th tick  | GPU, process, sensors, virt         |
| Slow   | Every 30th tick | Disk (the SMART ioctls), runtime    |

A skipped collector still contributes its last cached value, so the snapshot
shape never changes and a consumer never has to handle a missing field.

One consequence worth knowing: on the slow tier at a 1s interval, a disk needs
roughly a minute of uptime before two samples exist and rates become real.

Tiering is on for streaming (GUI and server) and off for one-shot CLI commands,
where you asked for a reading and should get a fresh one.

### What a tick actually costs

Tiering means there is no such thing as "the cost of a tick": one tick in five
costs several times what the other four do. That distinction is not academic —
reading the *average* is what hid a two-day outage (issue #65), because a
service can sit well under a CPU allowance on average while every expensive
tick is a burst the allowance cannot absorb.

Sampled on ci-node-1 at 250 ms resolution, `cpu.stat` deltas in milliseconds of
CPU:

```
367 367 472 0 0 0 0 0 0 202 0 0 0 0 0 0 158 0 0 0 0 5 0 0 89 0 0 0 0 0 0 0
361 347 425 449 352 399 438 393 400 384 0 0 0 0 9 200 0 0 ...
      ^ the medium tick: ~3.9 s of CPU over 2.5 s of wall clock
```

The median sample is **zero**. Every 2 s a fast tick costs a couple of hundred
milliseconds, and every fifth one a medium tick spends seconds — using more than
one core, because collectors overlap.

Per-collector figures come from `make bench-collector`, which reads the real
host and reports the process count beside every result. **They are comparable
only within one host**; quoting one machine's numbers for another is the mistake
that put an Atom D510's figures in the deployment unit. On gpu-host (1070
processes):

| Tier | Collector | Per call | Allocated |
|---|---|---|---|
| Fast | host, cpu, network, load | under 1 ms each | small |
| Fast | memory | 8.6 ms | 226 KB |
| Medium | **process** | **70.8 ms** | **39 MB** |
| Medium | gpu | 62.3 ms | 173 KB |
| Medium | virt | 26.4 ms | 12.7 MB |
| Medium | sensor | 3.9 ms | 1.3 MB |
| Slow | disk (SMART) | 113.9 ms | 1.8 MB |
| Slow | runtime | 3.6 ms | 52 KB |

Weighted by how often each tier runs, the process walk is the largest single
consumer of CPU on a busy host, and by a wide margin the largest consumer of
*memory*: 39 MB of allocation every medium tick, which is most of the garbage
the service produces. `ProcessCollector` and `VirtCollector` each walk the whole
process table separately, and identity fields that cannot change for the life of
a PID are re-read on every walk.

See `pkg/collector/gopsutil.go` for the one setting that halved both of them,
and why it cannot be covered by a test.

### Host identity is read once, not every tick

`HostCollector` used to call `host.Info()` on every tick. That call **counts
every entry in `/proc`** to fill a `Procs` field nothing here reads — a second
walk of the process table, every two seconds, for a number thrown away. On mon0
the collector cost **41 ms per tick on the host with the fewest processes**,
against 0.84 ms on the host with the most.

Board, BIOS, kernel version, kernel architecture and boot time cannot change
while the running kernel is up, so they are read once in the constructor and
carried forward. Measured: **777.7 us to 20.1 us per collection, 39x.**

Three fields are still read every tick, and the boundary is the point:

| field | why it stays live |
|---|---|
| `Hostname` | `hostnamectl` can change it while the service runs |
| `Uptime` | it is the field's entire purpose |
| `Platform`, `PlatformVersion` | `/etc/os-release` is rewritten by a distribution upgrade |

Caching the distribution strings would be defensible — the running system is
still the old one — but that changes **how fresh a field is**, which is a
decision about what the dashboard promises rather than a saving to be taken
quietly.

**That sentence used to end "`PlatformInformation` costs a couple of file reads
and does not count processes", and it was wrong on two of ten hosts.** gopsutil
reads `/etc/lsb-release` when it exists and otherwise **forks
`/usr/bin/lsb_release`**, a Python script on Debian. fs and mon0 are the only
two nodes without that file, and they were the only two whose `HostCollector`
cost tens of milliseconds — 38.65 ms on fs, 19.33 ms/s, **29% of that machine's
entire CPU**, spent starting an interpreter to report a distribution name. Ten
hosts, no exceptions.

The cost was measured on a workstation that has the file and generalised to the
fleet, which is the same error as the Atom D510 figures in the deployment unit.

`platformInformation` in `host.go` now short-circuits exactly the case where
gopsutil would fork **and** its answer is fully determined by files that can be
read: no `/etc/lsb-release`, an `lsb_release` binary present, `ID=debian` in
os-release, and no `raspi-config`. Everything else calls gopsutil unchanged.
Measured on fs: **38.65 ms to 0.19 ms, 206x**, with the reported values
identical — `debian 12.13`, the point release from `/etc/debian_version`, which
is what gopsutil returns and is *not* what os-release's `VERSION_ID` says.

**Split on the property, not on the path.** The same sysfs tree holds fixed
identity and live state side by side: DIMM *topology* cannot change, a DIMM
*temperature* changes constantly, and both live under the same directory.
"It is under `/sys`" is not a reason to cache anything.

### The DIMM inventory is read once; its temperatures are not

`MemoryCollector` reads the module list — slot, size, speed, manufacturer, part
number — from the SMBIOS type-17 table, which firmware generates at boot and
never rewrites. It is read in the constructor.

| host | before | after | |
|---|---|---|---|
| gpu-host (1070 procs, 24 cores) | 8.32 ms | 2.72 ms | −67% |
| fs (119 procs, 4 cores) | **34.74 ms** | **1.20 ms** | **−96.6%** |

The saving is larger on the smaller machine, which by now is the expected shape:
this cost was never in the process table, and the modest hosts pay most of it.

Temperatures come from hwmon and are read every tick, applied onto a **copy** of
the inventory. The copy is the correctness of the whole arrangement: writing
into the cached slice would leave the previous tick's value on a module whose
sensor has stopped answering, so **a temperature that is no longer being
measured would go on being displayed**, indistinguishable from one that is.
There is a test that removes the sensor between two collections and asserts the
reading goes to zero while the module stays; it fails if the copy is removed.

**What is deliberately still re-scanned every tick:** which hwmon devices *are*
DIMM sensors. That discovery is about a third of the collector and looks like
the same kind of invariant — a `name` file under sysfs, read to decide what to
read next. It is not one. Sensors appear *after* startup: this package's own
probe runs `modprobe jc42`/`spd5118` and instantiates spd5118 i2c devices, and
an operator can load a module by hand at any time. Caching the discovery would
mean a sensor appearing later is never noticed until a restart — the dashboard
quietly ceasing to answer a question it promises to answer, and undetectably,
because nobody notices a sensor that never shows up.

**And not a rescan-when-empty fallback either.** That covers the case where *no*
sensors were found and misses the case where *one more* appeared, which is the
actual scenario — it would pass every test and make the gap look considered.

## Sensor merging

hwmon reports temperatures against `(package, core)` identifiers; `/proc/stat`
reports usage against logical CPU index. They are different namespaces. After
all sub-collectors run, `mergeSensorIntoCPUs` matches them up and fills in
`CPUInfo.TemperatureCelsius` and `CPUInfo.VoltageV`.

This is fiddlier than it sounds. `coretemp` numbers its hwmon inputs sparsely —
`temp1`, `temp2`, `temp6`, `temp10` on a 24-thread part — so scanning
sequentially and stopping at the first gap finds exactly one core's temperature
and silently drops the rest. The collector globs the directory instead.

## Thread safety

There are no mutexes in the collection path. Each sub-collector stores its
latest result behind an `atomic.Pointer`, and the tick counter is an
`atomic.Uint64`. A reader always gets a complete, internally consistent result —
just possibly one tick old, which is the correct trade for a monitor.
